package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// transcoder feeds a Protect channel in a codec the doorbell does not send.
// It is the source's consumer: the video goes through ffmpeg (MPEG-TS in and
// out, so timestamps survive), the audio is handed to the Protect consumer
// unchanged.
//
// It exists for Protect's "enhanced encoding": once a camera is switched to
// H.265, Protect expects every channel in H.265 and drops H.264 segments, but
// Reolink sub streams are always H.264. The sub stream is small (640x480), so
// encoding it costs a fraction of one core.
type transcoder struct {
	core.Connection

	out   *unifi.Consumer
	ts    *mpegts.Consumer
	codec string // target: "h264" or "h265"
	fps   int

	mu    sync.Mutex
	video bool // a video track reached the MPEG-TS muxer
}

func newTranscoder(out *unifi.Consumer, codec string, fps int) *transcoder {
	return &transcoder{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "transcode",
			Medias: []*core.Media{
				{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecH264}, {Name: core.CodecH265}}},
				{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecAAC}}},
			},
		},
		out:   out,
		ts:    mpegts.NewConsumer(),
		codec: codec,
		fps:   fps,
	}
}

func (t *transcoder) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	if media.Kind == core.KindAudio {
		return t.out.AddTrack(media, codec, track)
	}
	t.mu.Lock()
	t.video = true
	t.mu.Unlock()
	return t.ts.AddTrack(media, codec, track)
}

func (t *transcoder) hasVideo() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.video
}

func (t *transcoder) Stop() error {
	_ = t.ts.Stop()
	return t.Connection.Stop()
}

// ffmpegArgs encodes MPEG-TS video from stdin to the target codec on stdout:
// low latency, no B-frames, a keyframe every second with the parameter sets
// repeated, so Protect can start a stream at any keyframe.
func ffmpegArgs(codec string, fps int) []string {
	if fps <= 0 {
		fps = 15
	}
	gop := strconv.Itoa(fps)
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer",
		"-probesize", "32768", "-analyzeduration", "500000", "-f", "mpegts", "-i", "pipe:0", "-map", "0:v:0"}
	if codec == "h265" {
		args = append(args, "-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency",
			"-x265-params", "log-level=error:bframes=0:repeat-headers=1:keyint="+gop+":min-keyint="+gop)
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-bf", "0", "-g", gop, "-keyint_min", gop)
	}
	return append(args, "-pix_fmt", "yuv420p", "-f", "mpegts", "pipe:1")
}

// run starts ffmpeg and attaches its video to the Protect consumer. It
// returns once the video track is attached (or failed); the pipeline then
// runs until ctx ends or ffmpeg exits, after which the consumer is stopped.
func (t *transcoder) run(ctx context.Context, ffmpeg string) error {
	cmd := exec.CommandContext(ctx, ffmpeg, ffmpegArgs(t.codec, t.fps)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tailBuffer{max: 2048}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	go func() {
		_, _ = t.ts.WriteTo(stdin)
		_ = stdin.Close()
	}()
	fail := func(err error) error {
		_ = t.ts.Stop()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if msg := stderr.String(); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}

	prod, err := mpegts.Open(stdout)
	if err != nil {
		return fail(fmt.Errorf("ffmpeg output: %w", err))
	}
	var recv *core.Receiver
	for _, media := range prod.Medias {
		if media.Kind == core.KindVideo && len(media.Codecs) > 0 {
			recv, _ = prod.GetTrack(media, media.Codecs[0])
			if err = t.out.AddTrack(t.out.Medias[0], media.Codecs[0], recv); err != nil {
				return fail(err)
			}
			break
		}
	}
	if recv == nil {
		return fail(errors.New("ffmpeg output has no video"))
	}

	go func() {
		err := prod.Start()
		_ = t.ts.Stop()
		_ = cmd.Wait()
		if ctx.Err() == nil {
			if msg := stderr.String(); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			log.Debug().Err(err).Msg("[bridge] transcoder stopped")
		}
		_ = t.out.Stop()
	}()
	return nil
}

// tailBuffer keeps the last max bytes written (ffmpeg's error output).
type tailBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (w *tailBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b = append(w.b, p...)
	if len(w.b) > w.max {
		w.b = w.b[len(w.b)-w.max:]
	}
	return len(p), nil
}

func (w *tailBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.b)
}

var _ io.Writer = (*tailBuffer)(nil)
