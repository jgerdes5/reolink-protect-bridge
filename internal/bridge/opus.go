package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// opusEncoder turns a source's AAC audio into Opus, which Protect's live view
// plays; the doorbell only has AAC. One ffmpeg per source feeds all its
// Protect channels: ADTS in, Ogg Opus out, 20 ms per packet.
type opusEncoder struct {
	core.Connection

	adts *aac.Consumer

	mu   sync.Mutex
	outs []*opusOut
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

// opusOut feeds one channel from its own queue, so a slow Protect socket
// only delays (and, when full, drops) its own Opus, not the other channels'.
type opusOut struct {
	c  *unifi.Consumer
	ch chan opusPacket
}

type opusPacket struct {
	data []byte
	pos  uint32
	at   time.Time
}

const opusQueue = 50 // packets: 1 s

func (o *opusEncoder) join(c *unifi.Consumer) {
	out := &opusOut{c: c, ch: make(chan opusPacket, opusQueue)}
	go func() {
		for p := range out.ch {
			c.WriteOpus(p.data, p.pos, p.at)
		}
	}()
	o.mu.Lock()
	o.outs = append(o.outs, out)
	o.mu.Unlock()
}

func (o *opusEncoder) leave(c *unifi.Consumer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	outs := make([]*opusOut, 0, len(o.outs))
	for _, x := range o.outs {
		if x.c == c {
			close(x.ch)
		} else {
			outs = append(outs, x)
		}
	}
	o.outs = outs
}

func (o *opusEncoder) deliver(p opusPacket) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, out := range o.outs {
		select {
		case out.ch <- p:
		default: // that channel is stuck: drop rather than hold up the others
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

// opusArgs: ADTS from stdin to Ogg Opus on stdout, 16 kHz mono in 20 ms
// packets like a G4, one Ogg page per packet. The tiny probe size matters:
// with ffmpeg's usual probing the demuxer keeps about a second of audio queued.
func opusArgs(encoder string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay",
		"-probesize", "32", "-analyzeduration", "0", "-f", "aac", "-i", "pipe:0",
		"-map", "0:a:0", "-ac", "1", "-ar", "16000", "-c:a", encoder, "-b:a", "64k"}
	if encoder == "libopus" {
		args = append(args, "-frame_duration", "20", "-application", "audio", "-compression_level", "0")
	} else {
		args = append(args, "-strict", "-2")
	}
	return append(args, "-flush_packets", "1", "-page_duration", "20000", "-f", "ogg", "pipe:1")
}

// opusCodec picks ffmpeg's Opus encoder: libopus, else the built-in one.
func opusCodec(ffmpeg string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return "", fmt.Errorf("ffmpeg -encoders: %w", err)
	}
	switch s := string(out); {
	case strings.Contains(s, " libopus "):
		return "libopus", nil
	case strings.Contains(s, " opus "):
		return "opus", nil
	}
	return "", errNoOpusEncoder
}

// errNoOpusEncoder is the only answer that is kept: other failures (ffmpeg
// briefly missing during an upgrade) are asked again next time.
var errNoOpusEncoder = errors.New("ffmpeg has no Opus encoder")

// opusEncoderName returns ffmpeg's Opus encoder, asking ffmpeg once. Must be
// called with b.omu held.
func (b *Bridge) opusEncoderName() (string, error) {
	if b.opusErr != nil || b.opusEnc != "" {
		return b.opusEnc, b.opusErr
	}
	enc, err := opusCodec(b.opts.FFmpeg)
	switch {
	case errors.Is(err, errNoOpusEncoder):
		b.opusErr = err
		log.Info().Err(err).Msg("[bridge] no Opus audio for Protect")
	case err != nil:
		log.Warn().Err(err).Msg("[bridge] can't check ffmpeg's Opus encoder, trying again later")
	default:
		b.opusEnc = enc
	}
	st := opusStatus{}
	switch {
	case b.opusErr != nil:
		st = opusStatus{state: "off", err: b.opusErr.Error()}
	case b.opusEnc != "":
		st = opusStatus{state: "on"}
	}
	b.opusState.Store(&st)
	if b.proto != nil && enc != "" {
		b.proto.SetOpus(true) // read by Protect at the next connection
	}
	return enc, err
}

type opusStatus struct{ state, err string }

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
	lag := &lagMeter{}
	return readOggPackets(bufio.NewReaderSize(r, 64<<10), func(pkt []byte) {
		n++
		if n <= 2 { // OpusHead, OpusTags
			return
		}
		at, ok := o.capturedAt(pos)
		if !ok {
			at = time.Now()
		}
		lag.add(time.Since(at))
		o.deliver(opusPacket{data: pkt, pos: uint32(pos), at: at})
		pos += uint64(opusSamples(pkt))
	})
}

// lagMeter logs once how far Opus trails the doorbell's audio (ffmpeg's
// delay): Protect's live view waits for audio, so a large lag delays it.
type lagMeter struct {
	n    int
	lags []time.Duration
	done bool
}

func (m *lagMeter) add(d time.Duration) {
	if m.done {
		return
	}
	if m.n++; m.n <= 100 { // the first 2 s, while ffmpeg starts
		return
	}
	if m.lags = append(m.lags, d); len(m.lags) < 250 {
		return
	}
	slices.Sort(m.lags)
	med := m.lags[len(m.lags)/2]
	ev := log.Info()
	if med > 300*time.Millisecond {
		ev = log.Warn()
	}
	ev.Msgf("[bridge] Opus audio is %d ms behind the doorbell's audio", med.Milliseconds())
	m.lags, m.done = nil, true
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
	encoder, err := b.opusEncoderName()
	if err != nil {
		return nil, err
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
		// bounded: omu is held, and a reconnecting source would block every channel
		addCtx, cancelAdd := context.WithTimeout(ctx, 5*time.Second)
		err := src.AddConsumer(addCtx, enc)
		cancelAdd()
		if err != nil {
			return nil, err // no audio on this source
		}
		runCtx, cancel := context.WithCancel(context.Background())
		enc.cancel = cancel
		if err := enc.run(runCtx, b.opts.FFmpeg, encoder); err != nil {
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
		if so.users == 0 {
			if b.opus[src.Name] == so {
				delete(b.opus, src.Name)
			}
			// also when a crashed encoder was already replaced in the map
			src.RemoveConsumer(so.enc)
			_ = so.enc.Stop()
		}
	}, nil
}

// checkOpus tells Protect whether the camera sends Opus, which ffmpeg can only
// do with an Opus encoder.
func (b *Bridge) checkOpus() {
	b.omu.Lock()
	defer b.omu.Unlock()
	enc, _ := b.opusEncoderName()
	if b.proto != nil {
		b.proto.SetOpus(enc != "")
	}
}

// opusErrSnapshot is the permanent "no Opus encoder" error, already logged.
func (b *Bridge) opusErrSnapshot() error {
	b.omu.Lock()
	defer b.omu.Unlock()
	return b.opusErr
}
