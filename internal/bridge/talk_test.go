package bridge

import (
	"bytes"
	"context"
	"io"
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

// opusPackets encodes a 1 s tone as Opus 24 kHz mono in 20 ms packets, as
// Protect sends HomeKit talkback (one packet per RTP datagram).
func opusPackets(t *testing.T) [][]byte {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=24000", "-t", "1",
		"-ac", "1", "-c:a", "libopus", "-frame_duration", "20", "-f", "ogg", "pipe:1").Output()
	if err != nil {
		t.Skip("ffmpeg with libopus not available:", err)
	}
	var pkts [][]byte
	_ = readOggPackets(bytes.NewReader(out), func(p []byte) { pkts = append(pkts, p) })
	require.Greater(t, len(pkts), 2)
	return pkts[2:] // without OpusHead and OpusTags
}

func TestTalkbackOpus(t *testing.T) {
	pkts := opusPackets(t)
	frames := adtsFrames(t)

	speaker := &speakerStats{}
	src := source.New("main", func() (source.Producer, error) { return newFakeSpeaker(speaker), nil }, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	tk := &talker{key: "test", src: src, ffmpeg: "ffmpeg", idle: 2 * time.Second}
	defer tk.close()
	played := func(min int) func() bool {
		return func() bool {
			speaker.mu.Lock()
			defer speaker.mu.Unlock()
			return speaker.samples > min
		}
	}

	for _, p := range pkts {
		tk.feedOpus(p)
		time.Sleep(5 * time.Millisecond)
	}
	require.Eventually(t, played(12000), 5*time.Second, 50*time.Millisecond)

	// AAC right after Opus ends the Opus session and plays in a new one
	speaker.mu.Lock()
	speaker.samples = 0
	speaker.mu.Unlock()
	for _, f := range frames {
		tk.feed(f)
		time.Sleep(5 * time.Millisecond)
	}
	require.Eventually(t, played(12000), 5*time.Second, 50*time.Millisecond)
}

func TestOggWriterRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	o := &oggWriter{w: &buf, serial: 1}
	require.NoError(t, o.opusHeaders())
	big := bytes.Repeat([]byte{0xfc}, 600) // spans several lacing segments
	require.NoError(t, o.packet([]byte{0xfc, 1, 2}))
	require.NoError(t, o.packet(big))
	var got [][]byte
	require.ErrorIs(t, readOggPackets(&buf, func(p []byte) { got = append(got, p) }), io.EOF)
	require.Len(t, got, 4)
	require.Equal(t, "OpusHead", string(got[0][:8]))
	require.Equal(t, big, got[3])
}
