package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// opusEncoder turns a source's AAC audio into Opus for Protect. A UniFi
// camera sends Opus next to AAC when Protect asks for it (withOpus), and
// Protect's live view plays Opus; the doorbell only has AAC. One ffmpeg per
// source feeds every Protect channel of it. It is the source's audio
// consumer: ADTS goes in, Ogg Opus comes out, 20 ms per packet.
type opusEncoder struct {
	core.Connection

	adts *aac.Consumer

	mu   sync.Mutex
	outs []*unifi.Consumer
	// arrivals of the AAC frames written to ffmpeg, to stamp each Opus packet
	// with the time its sound arrived (ffmpeg delays it)
	frames []aacFrame
	done   chan struct{}
	cancel context.CancelFunc // ends ffmpeg
}

type aacFrame struct {
	start uint64 // first sample, in 48 kHz samples since the first frame
	at    time.Time
}

const aacFrameSamples48 = 1024 * 48000 // per AAC frame: 1024 samples at the codec rate, scaled below

func newOpusEncoder() *opusEncoder {
	return &opusEncoder{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "opus",
			Medias:     []*core.Media{{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecAAC}}}},
		},
		adts: aac.NewConsumer(),
		done: make(chan struct{}),
	}
}

func (o *opusEncoder) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	return o.adts.AddTrack(media, codec, track)
}

func (o *opusEncoder) Stop() error {
	_ = o.adts.Stop()
	if o.cancel != nil {
		o.cancel()
	}
	return o.Connection.Stop()
}

func (o *opusEncoder) join(c *unifi.Consumer) {
	o.mu.Lock()
	o.outs = append(o.outs, c)
	o.mu.Unlock()
}

func (o *opusEncoder) leave(c *unifi.Consumer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, x := range o.outs {
		if x == c {
			o.outs = append(o.outs[:i], o.outs[i+1:]...)
			return
		}
	}
}

// adtsWriter notes when each AAC frame arrived on its way to ffmpeg.
type adtsWriter struct {
	o    *opusEncoder
	w    io.Writer
	rate uint64 // AAC sample rate
	pos  uint64 // 48 kHz samples written so far
}

func (a *adtsWriter) Write(p []byte) (int, error) {
	// aac.Consumer writes whole ADTS frames, one or more per call
	now := time.Now()
	a.o.mu.Lock()
	for b := p; aac.IsADTS(b); {
		n := int(aac.ReadADTSSize(b))
		if n <= 0 || n > len(b) {
			break
		}
		if a.rate == 0 {
			if codec := aac.ADTSToCodec(b); codec != nil && codec.ClockRate > 0 {
				a.rate = uint64(codec.ClockRate)
			} else {
				a.rate = 16000
			}
		}
		a.o.frames = append(a.o.frames, aacFrame{start: a.pos, at: now})
		a.pos += aacFrameSamples48 / a.rate
		b = b[n:]
	}
	if len(a.o.frames) > 512 { // ~30 s at 16 kHz; ffmpeg lags far less
		a.o.frames = append(a.o.frames[:0], a.o.frames[len(a.o.frames)-256:]...)
	}
	a.o.mu.Unlock()
	return a.w.Write(p)
}

// capturedAt is when the sound at 48 kHz sample pos arrived as AAC.
func (o *opusEncoder) capturedAt(pos uint64) (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.frames) - 1; i >= 0; i-- {
		f := o.frames[i]
		if f.start <= pos {
			return f.at.Add(time.Duration(pos-f.start) * time.Second / 48000), true
		}
	}
	return time.Time{}, false
}

// opusArgs: ADTS from stdin to Ogg Opus on stdout, 48 kHz mono, 20 ms
// packets, one Ogg page per packet so nothing waits in the muxer.
func opusArgs(encoder string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer",
		"-probesize", "4096", "-analyzeduration", "0", "-f", "aac", "-i", "pipe:0",
		"-map", "0:a:0", "-ac", "1", "-ar", "48000", "-c:a", encoder, "-b:a", "64k"}
	if encoder == "libopus" {
		args = append(args, "-frame_duration", "20", "-application", "lowdelay")
	} else {
		args = append(args, "-strict", "-2")
	}
	return append(args, "-flush_packets", "1", "-page_duration", "20000", "-f", "ogg", "pipe:1")
}

// opusCodec picks ffmpeg's Opus encoder: libopus, else the built-in one.
func opusCodec(ffmpeg string) (string, error) {
	out, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", err
	}
	switch s := string(out); {
	case strings.Contains(s, " libopus "):
		return "libopus", nil
	case strings.Contains(s, " opus "):
		return "opus", nil
	}
	return "", errors.New("ffmpeg has no Opus encoder")
}

// run starts ffmpeg; it runs until ctx ends or ffmpeg exits (done closes).
func (o *opusEncoder) run(ctx context.Context, ffmpeg, encoder string) error {
	cmd := exec.CommandContext(ctx, ffmpeg, opusArgs(encoder)...)
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
		_, _ = o.adts.WriteTo(&adtsWriter{o: o, w: stdin})
		_ = stdin.Close()
	}()
	go func() {
		defer close(o.done)
		err := o.readOgg(stdout)
		_ = o.adts.Stop()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if ctx.Err() == nil {
			if msg := stderr.String(); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			log.Warn().Err(err).Msg("[bridge] opus encoder stopped")
		}
	}()
	return nil
}

// readOgg hands each Opus packet to the consumers.
func (o *opusEncoder) readOgg(r io.Reader) error {
	var pos uint64 // 48 kHz samples
	n := 0
	return readOggPackets(bufio.NewReaderSize(r, 64<<10), func(pkt []byte) {
		n++
		if n <= 2 { // OpusHead, OpusTags
			return
		}
		at, ok := o.capturedAt(pos)
		if !ok {
			at = time.Now()
		}
		o.mu.Lock()
		outs := o.outs
		o.mu.Unlock()
		for _, c := range outs {
			c.WriteOpus(pkt, uint32(pos), at)
		}
		pos += uint64(opusSamples(pkt))
	})
}

// readOggPackets splits an Ogg stream into packets (RFC 3533).
func readOggPackets(r io.Reader, fn func([]byte)) error {
	var hdr [27]byte
	var lacing [255]byte
	var pkt []byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		if string(hdr[:4]) != "OggS" {
			return errors.New("ogg: lost sync")
		}
		nseg := int(hdr[26])
		if _, err := io.ReadFull(r, lacing[:nseg]); err != nil {
			return err
		}
		size := 0
		for _, l := range lacing[:nseg] {
			size += int(l)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return err
		}
		if hdr[5]&1 == 0 { // not a continuation: drop a dangling partial packet
			pkt = pkt[:0]
		}
		off := 0
		for _, l := range lacing[:nseg] {
			pkt = append(pkt, body[off:off+int(l)]...)
			off += int(l)
			if l < 255 {
				fn(append([]byte(nil), pkt...))
				pkt = pkt[:0]
			}
		}
	}
}

// opusSamples is a packet's duration in 48 kHz samples, from its TOC byte
// (RFC 6716 3.1).
func opusSamples(pkt []byte) int {
	if len(pkt) == 0 {
		return 0
	}
	toc := pkt[0]
	cfg := int(toc >> 3)
	var frame int // in 48 kHz samples
	switch {
	case cfg < 12: // SILK 10/20/40/60 ms
		frame = [4]int{480, 960, 1920, 2880}[cfg%4]
	case cfg < 16: // hybrid 10/20 ms
		frame = [2]int{480, 960}[cfg%2]
	default: // CELT 2.5/5/10/20 ms
		frame = [4]int{120, 240, 480, 960}[cfg%4]
	}
	switch toc & 3 {
	case 0:
		return frame
	case 1, 2:
		return 2 * frame
	}
	if len(pkt) < 2 {
		return frame
	}
	return int(pkt[1]&0x3f) * frame
}

// sharedOpus is one source's encoder and how many channels use it.
type sharedOpus struct {
	enc   *opusEncoder
	users int
}

// opusFor joins c to the source's Opus encoder, starting it if needed; the
// returned function leaves (the last one stops ffmpeg). Without an Opus
// encoder in ffmpeg the stream simply has no Opus.
func (b *Bridge) opusFor(ctx context.Context, src *source.Source, c *unifi.Consumer) (func(), error) {
	b.omu.Lock()
	defer b.omu.Unlock()
	if b.opusErr != nil {
		return nil, b.opusErr
	}
	if b.opusEnc == "" {
		enc, err := opusCodec(b.opts.FFmpeg)
		if err != nil {
			b.opusErr = err
			log.Info().Err(err).Msg("[bridge] no Opus audio for Protect")
			return nil, err
		}
		b.opusEnc = enc
	}
	if b.opus == nil {
		b.opus = map[string]*sharedOpus{}
	}
	so := b.opus[src.Name]
	if so != nil {
		select {
		case <-so.enc.done: // ffmpeg ended: start a new one
			delete(b.opus, src.Name)
			so = nil
		default:
		}
	}
	if so == nil {
		enc := newOpusEncoder()
		if err := src.AddConsumer(ctx, enc); err != nil {
			return nil, err // no audio on this source
		}
		runCtx, cancel := context.WithCancel(context.Background())
		enc.cancel = cancel
		if err := enc.run(runCtx, b.opts.FFmpeg, b.opusEnc); err != nil {
			cancel()
			src.RemoveConsumer(enc)
			return nil, err
		}
		so = &sharedOpus{enc: enc}
		b.opus[src.Name] = so
	}
	so.users++
	c.EnableOpus()
	so.enc.join(c)
	return func() {
		so.enc.leave(c)
		b.omu.Lock()
		defer b.omu.Unlock()
		so.users--
		if so.users == 0 && b.opus[src.Name] == so {
			delete(b.opus, src.Name)
			src.RemoveConsumer(so.enc)
			_ = so.enc.Stop()
		}
	}, nil
}

// opusErrSnapshot is the permanent "no Opus encoder" error, already logged.
func (b *Bridge) opusErrSnapshot() error {
	b.omu.Lock()
	defer b.omu.Unlock()
	return b.opusErr
}
