package bridge

import (
	"context"
	"encoding/binary"
	"os/exec"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestOpusSamples(t *testing.T) {
	require.Equal(t, 960, opusSamples([]byte{0xb8}))         // CELT WB 20 ms, as a G4 sends (16 kHz)
	require.Equal(t, 480, opusSamples([]byte{0x00}))         // SILK NB 10 ms (config 0)
	require.Equal(t, 960, opusSamples([]byte{0x08}))         // SILK NB 20 ms (config 1)
	require.Equal(t, 2*960, opusSamples([]byte{0xb9}))       // two frames
	require.Equal(t, 3*960, opusSamples([]byte{0xbb, 0x03})) // code 3, 3 frames
	require.Equal(t, 0, opusSamples(nil))
}

// A stream with AAC gets Opus too: ffmpeg encodes the source's audio once,
// the stream carries the config tag cf 00 03 02 and 20 ms packets with 48 kHz
// trailers, close to the AAC frames in time.
func TestOpusStream(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := opusCodec(ffmpeg); err != nil {
		t.Skip(err)
	}
	src := source.New("main", func() (source.Producer, error) {
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000",
			"-t", "20", "-c:v", "libx264", "-preset", "ultrafast", "-g", "15", "-bf", "0",
			// no audio interleaving delay, so AAC arrives frame by frame as
			// from the doorbell (the muxer otherwise bunches ~0.7 s)
			"-c:a", "aac", "-b:a", "32k", "-muxdelay", "0", "-muxpreload", "0", "-pes_payload_size", "0",
			"-f", "mpegts", "pipe:1")
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
	cons := unifi.NewConsumer(unifi.StreamOptions{StreamName: "s", FPS: 15})
	require.NoError(t, src.AddConsumer(ctx, cons))
	release, err := b.opusFor(ctx, src, cons)
	require.NoError(t, err)
	out := &lockedBuffer{}
	go func() { _, _ = cons.WriteTo(out) }()

	type tag struct {
		typ   byte
		ts    uint32
		clock uint32
		pay   []byte
	}
	parse := func(b []byte) (tags []tag) {
		for i := 13; i+15 < len(b); {
			size := int(b[i+1])<<16 | int(b[i+2])<<8 | int(b[i+3])
			if i+11+size+20 > len(b) {
				break
			}
			ts := uint32(b[i+4])<<16 | uint32(b[i+5])<<8 | uint32(b[i+6]) | uint32(b[i+7])<<24
			tr := b[i+11+size+4:]
			tags = append(tags, tag{b[i], ts, binary.BigEndian.Uint32(tr), b[i+11 : i+11+size]})
			i += 11 + size + 4 + 16
		}
		return
	}
	var opus, aacTags []tag
	require.Eventually(t, func() bool {
		opus, aacTags = opus[:0], aacTags[:0]
		for _, tg := range parse(out.Bytes()) {
			switch {
			case tg.typ == 10:
				opus = append(opus, tg)
			case tg.typ == 8 && len(tg.pay) > 1 && tg.pay[1] == 1:
				aacTags = append(aacTags, tg)
			}
		}
		return len(opus) > 150 && len(aacTags) > 30
	}, 20*time.Second, 50*time.Millisecond)

	// lined up with the AAC track despite ffmpeg's delay: the Opus packet at
	// the time of an AAC frame is within a frame or two of it
	ref := aacTags[15].ts
	near := int64(1 << 30)
	for _, tg := range opus[1:] {
		near = min(near, abs64(int64(tg.ts)-int64(ref)))
	}
	require.Less(t, near, int64(150), "Opus %d ms off the AAC timeline", near)

	// and it arrives promptly: each Opus tag is written within a few hundred
	// ms of the newest AAC tag before it (ffmpeg's probing once held a second)
	var lastAAC uint32
	lag := int64(0)
	var lags []int64
	for _, tg := range parse(out.Bytes()) {
		switch {
		case tg.typ == 8 && len(tg.pay) > 1 && tg.pay[1] == 1:
			lastAAC = tg.ts
		case tg.typ == 10 && len(tg.pay) > 4 && lastAAC > 0:
			lags = append(lags, int64(lastAAC)-int64(tg.ts))
			lag = max(lag, int64(lastAAC)-int64(tg.ts))
		}
	}
	t.Logf("Opus lag behind AAC (ms): %v", lags[len(lags)/2:])
	require.Less(t, lag, int64(250), "Opus written %d ms behind the AAC", lag)

	require.Equal(t, []byte{0xcf, 0, 3, 2}, opus[0].pay) // config first
	require.Equal(t, uint32(48000), opus[1].clock)
	for _, tg := range opus[1:] {
		require.Equal(t, 960, opusSamples(tg.pay), "20 ms packets")
		require.Equal(t, byte(0xb8), tg.pay[0]&0xf8, "CELT wideband 20 ms, matching the 16 kHz config")
	}
	// 20 ms apart on the timeline (the clock may nudge by a millisecond)
	last := opus[len(opus)-1].ts
	n := len(opus) - 2
	require.InDelta(t, float64(n*20), float64(last-opus[1].ts), float64(n)*2)

	// one encoder per source; the last user stops it
	b.omu.Lock()
	enc := b.opus["main"].enc
	b.omu.Unlock()
	release()
	select {
	case <-enc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("ffmpeg still running")
	}
	_ = cons.Stop()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
