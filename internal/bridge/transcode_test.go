package bridge

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
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

// TestTranscoderH264ToH265 runs a generated H.264 stream through the real
// ffmpeg and checks that Protect gets H.265 video tags (codec id 8).
func TestTranscoderH264ToH265(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, _ := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg without libx265")
	}

	// an H.264 sub stream like the doorbell's, as go2rtc tracks
	gen := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
		"-f", "lavfi", "-i", "testsrc=size=640x480:rate=15", "-t", "4",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "15", "-bf", "0", "-f", "mpegts", "pipe:1")
	stdout, err := gen.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, gen.Start())
	defer func() { _ = gen.Process.Kill(); _ = gen.Wait() }()
	src, err := mpegts.Open(stdout)
	require.NoError(t, err)
	media := src.Medias[0]
	require.Equal(t, core.CodecH264, media.Codecs[0].Name)
	track, err := src.GetTrack(media, media.Codecs[0])
	require.NoError(t, err)

	cons := unifi.NewConsumer(unifi.StreamOptions{StreamName: "s", Channel: 1, FPS: 15})
	tr := newTranscoder(cons, "h265", 15)
	require.NoError(t, tr.AddTrack(tr.Medias[0], media.Codecs[0], track))
	go func() { _ = src.Start() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, tr.run(ctx, ffmpeg))
	video, _ := cons.TrackCodecs()
	require.Equal(t, core.CodecH265, video.Name)

	out := &lockedBuffer{}
	go func() { _, _ = cons.WriteTo(out) }()

	hevcFrames := func() int {
		b := out.Bytes()
		n := 0
		for i := 13; i+15 < len(b); {
			size := int(b[i+1])<<16 | int(b[i+2])<<8 | int(b[i+3])
			if b[i] == 9 && b[i+11]&0x0F == 8 {
				n++
			}
			i += 11 + size + 4 + 16 // PreviousTagSize and the UniFi trailer (flv.go)
		}
		return n
	}
	require.Eventually(t, func() bool { return hevcFrames() >= 5 }, 10*time.Second, 50*time.Millisecond)
	w, h := cons.Size()
	require.Equal(t, [2]int{640, 480}, [2]int{w, h})

	_ = cons.Stop()
	_ = tr.Stop()
}
