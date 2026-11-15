package bridge

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fakeSpeaker is a producer with only a backchannel, like the reolink producer
// with video=false&audio=false.
// A new instance per dial, like real producers; speakerStats is shared.
type fakeSpeaker struct {
	core.Connection
	*speakerStats
	done chan struct{}
}

type speakerStats struct {
	mu      sync.Mutex
	codec   *core.Codec
	samples int
}

func newFakeSpeaker(stats *speakerStats) *fakeSpeaker {
	f := &fakeSpeaker{speakerStats: stats, done: make(chan struct{})}
	f.Medias = []*core.Media{{
		Kind: core.KindAudio, Direction: core.DirectionSendonly,
		Codecs: []*core.Codec{ // reolink producer order: PCMA first
			{Name: core.CodecPCMA, ClockRate: 8000},
			{Name: core.CodecPCMU, ClockRate: 8000},
			{Name: core.CodecPCML, ClockRate: 16000},
		},
	}}
	return f
}

func (f *fakeSpeaker) AddTrack(_ *core.Media, codec *core.Codec, track *core.Receiver) error {
	f.mu.Lock()
	f.codec = codec
	f.mu.Unlock()
	sender := core.NewSender(f.Medias[0], codec)
	sender.Handler = func(pkt *core.Packet) {
		f.mu.Lock()
		f.samples += len(pkt.Payload) / 2
		f.mu.Unlock()
	}
	sender.HandleRTP(track)
	f.Senders = append(f.Senders, sender)
	return nil
}

func (f *fakeSpeaker) Start() error { <-f.done; return nil }
func (f *fakeSpeaker) Stop() error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

// adtsFrames encodes a 1 s tone as ADTS AAC 22.05 kHz mono, split into frames
// the way Protect sends them (one per datagram).
func adtsFrames(t *testing.T) [][]byte {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=22050", "-t", "1",
		"-ac", "1", "-c:a", "aac", "-f", "adts", "pipe:1").Output()
	if err != nil {
		t.Skip("ffmpeg not available:", err)
	}
	var frames [][]byte
	for len(out) > 7 {
		n := int(out[3]&3)<<11 | int(out[4])<<3 | int(out[5])>>5
		if n <= 7 || n > len(out) {
			break
		}
		frames = append(frames, out[:n])
		out = out[n:]
	}
	return frames
}

func TestTalkback(t *testing.T) {
	frames := adtsFrames(t)
	require.Greater(t, len(frames), 15)

	speaker := &speakerStats{}
	src := source.New("main", func() (source.Producer, error) { return newFakeSpeaker(speaker), nil }, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	tk := &talker{key: "test", src: src, ffmpeg: "ffmpeg", idle: 2 * time.Second}
	for _, f := range frames {
		tk.feed(f)
		time.Sleep(5 * time.Millisecond)
	}

	// 16 kHz PCM is preferred over the producer's first codec, PCMA 8 kHz
	require.Eventually(t, func() bool {
		speaker.mu.Lock()
		defer speaker.mu.Unlock()
		return speaker.samples > 12000 // most of 1 s at 16 kHz
	}, 5*time.Second, 50*time.Millisecond)
	speaker.mu.Lock()
	require.Equal(t, core.CodecPCML, speaker.codec.Name)
	require.Equal(t, uint32(16000), speaker.codec.ClockRate)
	speaker.mu.Unlock()

	// the session ends after talkIdle and the consumer is removed
	require.Eventually(t, func() bool {
		tk.mu.Lock()
		defer tk.mu.Unlock()
		return tk.ch == nil
	}, tk.idle+3*time.Second, 50*time.Millisecond)

	// talking again opens a new session that plays too
	speaker.mu.Lock()
	speaker.samples = 0
	speaker.mu.Unlock()
	for _, f := range frames {
		tk.feed(f)
		time.Sleep(5 * time.Millisecond)
	}
	require.Eventually(t, func() bool {
		speaker.mu.Lock()
		defer speaker.mu.Unlock()
		return speaker.samples > 12000
	}, 5*time.Second, 50*time.Millisecond)
}

func TestRawFormat(t *testing.T) {
	for codec, want := range map[string]string{
		core.CodecPCMA: "alaw", core.CodecPCMU: "mulaw", core.CodecPCML: "s16le", core.CodecPCM: "s16be",
	} {
		f, _ := rawFormat(&core.Codec{Name: codec})
		require.Equal(t, want, f)
	}
}
