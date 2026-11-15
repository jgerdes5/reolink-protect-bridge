package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// transcoder runs one ffmpeg pipeline that turns a source's video into a
// codec the doorbell does not send, shared by every Protect channel that
// needs it. MPEG-TS goes in and out, so timestamps survive; audio bypasses it
// (audioTap). Needed because Protect in H.265 mode drops H.264 segments, and
// Reolink sub streams are always H.264.
type transcoder struct {
	core.Connection

	ts    *mpegts.Consumer
	codec string // target: "h264" or "h265"
	fps   int

	mu       sync.Mutex
	video    bool             // a video track reached the MPEG-TS muxer
	outCodec *core.Codec      // ffmpeg's output, once running
	outs     []*core.Receiver // one per Protect channel
	done     chan struct{}    // closed when ffmpeg has stopped
}

func newTranscoder(codec string, fps int) *transcoder {
	return &transcoder{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "transcode",
			Medias: []*core.Media{
				{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecH264}, {Name: core.CodecH265}}},
			},
		},
		ts:    mpegts.NewConsumer(),
		codec: codec,
		fps:   fps,
		done:  make(chan struct{}),
	}
}

func (t *transcoder) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
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

// join attaches a Protect consumer to the output. Each consumer gets a
// receiver of its own, attached before any packet reaches it, so joining a
// running pipeline does not race with the packets of the others.
func (t *transcoder) join(cons *unifi.Consumer) (*core.Receiver, error) {
	t.mu.Lock()
	codec := t.outCodec
	t.mu.Unlock()
	if codec == nil {
		return nil, errors.New("transcoder not running")
	}
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
	r := core.NewReceiver(media, codec)
	if err := cons.AddTrack(cons.Medias[0], codec, r); err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.outs = append(t.outs, r)
	t.mu.Unlock()
	return r, nil
}

func (t *transcoder) leave(r *core.Receiver) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, o := range t.outs {
		if o == r {
			t.outs = append(t.outs[:i], t.outs[i+1:]...)
			return
		}
	}
}

func (t *transcoder) fanout(pkt *core.Packet) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.outs {
		r.WriteRTP(pkt)
	}
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

// run starts ffmpeg. It returns once ffmpeg's output is known (or failed);
// startCtx bounds that wait (ffmpeg is killed when it ends first). The
// pipeline then runs until ctx ends or ffmpeg exits, which closes done.
func (t *transcoder) run(startCtx, ctx context.Context, ffmpeg string) error {
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

	in := &pipeWriter{w: stdin, attached: make(chan struct{})}
	written := make(chan struct{})
	go func() {
		defer close(written)
		_, _ = t.ts.WriteTo(in)
		_ = stdin.Close()
	}()
	t.waitAttached(in, written)
	fail := func(err error) error {
		_ = t.ts.Stop()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(t.done)
		if msg := stderr.String(); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}

	// no output yet (a stalled source): killing ffmpeg ends mpegts.Open
	stopKill := context.AfterFunc(startCtx, func() { _ = cmd.Process.Kill() })
	prod, err := mpegts.Open(stdout)
	if !stopKill() {
		if err == nil {
			_ = prod.Stop()
		}
		return fail(fmt.Errorf("ffmpeg start: %w", context.Cause(startCtx)))
	}
	if err != nil {
		return fail(fmt.Errorf("ffmpeg output: %w", err))
	}
	var media *core.Media
	for _, m := range prod.Medias {
		if m.Kind == core.KindVideo && len(m.Codecs) > 0 {
			media = m
			break
		}
	}
	if media == nil {
		_ = prod.Stop()
		return fail(errors.New("ffmpeg output has no video"))
	}
	codec := media.Codecs[0]
	recv, _ := prod.GetTrack(media, codec)
	sender := core.NewSender(media, codec)
	sender.Handler = t.fanout
	sender.HandleRTP(recv)
	t.mu.Lock()
	t.outCodec = codec
	t.mu.Unlock()

	go func() {
		err := prod.Start()
		_ = t.ts.Stop()
		// ffmpeg may still run after a demux error: Wait alone would hang
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if ctx.Err() == nil {
			if msg := stderr.String(); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			log.Warn().Err(err).Msg("[bridge] transcoder stopped")
		}
		close(t.done)
	}()
	return nil
}

// pipeWriter is ffmpeg's stdin as the MPEG-TS consumer's output. It hides
// the pipe's Close: core.WriteBuffer.Close would otherwise only close the pipe
// and leave WriteTo waiting for a failed write, which a stalled source never
// makes. A zero-length write marks that the buffer writes here.
type pipeWriter struct {
	w        io.Writer
	attached chan struct{}
	once     sync.Once
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		p.once.Do(func() { close(p.attached) })
		return 0, nil
	}
	return p.w.Write(b)
}

// waitAttached returns once the MPEG-TS consumer writes to in (or WriteTo
// has ended). core.WriteBuffer.Close reads its writer without the lock that
// WriteTo sets it under: an empty write through the buffer takes that lock
// and reaches in only after the switch, so Stop is ordered after it.
func (t *transcoder) waitAttached(in *pipeWriter, written chan struct{}) {
	wb, ok := t.ts.Transport.(io.Writer)
	if !ok {
		return
	}
	for {
		_, _ = wb.Write(nil)
		select {
		case <-in.attached:
			return
		case <-written:
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// audioTap hands a source's audio to a Protect consumer whose video comes
// from a transcoder.
type audioTap struct {
	core.Connection
	out *unifi.Consumer
}

func newAudioTap(out *unifi.Consumer) *audioTap {
	return &audioTap{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "audio",
			Medias:     []*core.Media{{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecAAC}}}},
		},
		out: out,
	}
}

func (a *audioTap) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	return a.out.AddTrack(media, codec, track)
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
