package bridge

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.b.Bytes()...)
}

// tsProducer is a generated stream as a source producer (no backchannel).
type tsProducer struct {
	*mpegts.Producer
	cmd *exec.Cmd
}

func (p *tsProducer) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	return errors.New("no backchannel")
}

func (p *tsProducer) Stop() error {
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return p.Producer.Stop()
}

// tags counts a UniFi stream's video tags of an FLV codec id, and audio tags.
func tags(b []byte, codecID byte) (video, audio int) {
	for i := 13; i+15 < len(b); {
		size := int(b[i+1])<<16 | int(b[i+2])<<8 | int(b[i+3])
		switch {
		case b[i] == 9 && b[i+11]&0x0F == codecID:
			video++
		case b[i] == 8:
			audio++
		}
		i += 11 + size + 4 + 16 // PreviousTagSize and the UniFi trailer (flv.go)
	}
	return
}

// TestTranscoderShared runs a generated H.264 + AAC sub stream through the
// real ffmpeg for two Protect channels: one ffmpeg, both get H.265 video
// (codec id 8) and the source's audio.
func TestTranscoderShared(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, _ := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg without libx265")
	}

	src := source.New("sub", func() (source.Producer, error) {
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
			"-f", "lavfi", "-i", "testsrc=size=640x480:rate=15", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000",
			"-t", "20", "-c:v", "libx264", "-preset", "ultrafast", "-g", "15", "-bf", "0",
			"-c:a", "aac", "-b:a", "32k", "-f", "mpegts", "pipe:1")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err = cmd.Start(); err != nil {
			return nil, err
		}
		prod, err := mpegts.Open(stdout)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		return &tsProducer{Producer: prod, cmd: cmd}, nil
	}, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	b := &Bridge{opts: Options{FFmpeg: ffmpeg}}
	info := unifi.Channel{Width: 640, Height: 480, FPS: 15, Codec: "h264"}
	var outs []*lockedBuffer
	var conss []*unifi.Consumer
	for i, ch := range []string{"video2", "video3"} {
		cons := unifi.NewConsumer(unifi.StreamOptions{StreamName: ch, Channel: i + 1, FPS: 15})
		passthrough, err := b.attach(ctx, ch, src, cons, info, "h265")
		require.NoError(t, err)
		require.False(t, passthrough)
		video, audio := cons.TrackCodecs()
		require.Equal(t, core.CodecH265, video.Name)
		require.NotNil(t, audio)
		out := &lockedBuffer{}
		go func() { _, _ = cons.WriteTo(out) }()
		outs = append(outs, out)
		conss = append(conss, cons)
	}
	b.tmu.Lock()
	require.Len(t, b.transcodes, 1)
	st := b.transcodes["sub|h265"]
	require.Equal(t, 2, st.users)
	b.tmu.Unlock()

	for _, out := range outs {
		require.Eventually(t, func() bool {
			v, a := tags(out.Bytes(), 8)
			return v >= 5 && a >= 5
		}, 15*time.Second, 50*time.Millisecond)
	}
	w, h := conss[1].Size()
	require.Equal(t, [2]int{640, 480}, [2]int{w, h})

	// the last channel to stop ends ffmpeg
	_ = conss[0].Stop()
	select {
	case <-st.tr.done:
		t.Fatal("ffmpeg stopped while video3 still uses it")
	case <-time.After(100 * time.Millisecond):
	}
	_ = conss[1].Stop()
	select {
	case <-st.tr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("ffmpeg still running")
	}
	b.tmu.Lock()
	require.Empty(t, b.transcodes)
	b.tmu.Unlock()
}
