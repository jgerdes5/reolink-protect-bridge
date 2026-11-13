package unifi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/pion/rtp"
)

// Consumer streams one go2rtc stream (H.264 or H.265, optional AAC) to Protect
// as UniFi FLV. The bytes are only re-wrapped: no decoding, no encoding.
type Consumer struct {
	core.Connection

	opts StreamOptions

	mu        sync.Mutex
	wr        io.Writer
	mux       *muxer
	video     *core.Codec
	hevc      bool // video is H.265
	audio     *core.Codec
	started   bool // first keyframe sent
	vclock    trackClock
	aclock    trackClock
	oclock    trackClock    // Opus
	opus      bool          // EnableOpus
	vpos      time.Duration // timeline position of the last video frame
	spsIn     []byte        // last SPS seen and its rewritten form (fixSPS)
	spsOut    []byte
	buf       []byte // reused output buffer
	frames    int    // video frames received, also before streaming
	width     int    // from the SPS, once known
	height    int
	closed    bool
	err       error
	done      chan struct{}
	closer    atomic.Value // io.Closer of the writer, closed by Stop without c.mu
	onStop    []func()
	onStopRan bool      // OnStop callbacks have run
	keyWanted time.Time // Protect asked for a keyframe (KeyframeWanted)
	stopped   sync.Once

	now func() time.Time // for tests
}

// writeTimeout bounds a single write to Protect, so a stalled NVR socket ends
// the stream instead of blocking the sender forever.
const writeTimeout = 10 * time.Second

// maxBuffer is the largest output buffer kept for reuse (a 5 MP keyframe is
// a few hundred KB).
const maxBuffer = 2 << 20

// StreamOptions describe the channel a Consumer feeds.
type StreamOptions struct {
	StreamName string // from Protect's ChangeVideoSettings
	Channel    int    // 0, 1, 2 for video1, video2, video3
	FPS        int
	Bitrate    int // advertised bandwidth in bit/s
	// FFmpegMetadata sends ffmpeg's onMetaData (ECMA array) like unifi-cam-proxy,
	// instead of the AMF object a real camera sends.
	FFmpegMetadata bool
	// OnKeyframe, if set, reports how long after KeyframeWanted the next
	// keyframe went out (called with the consumer's lock held: keep it short).
	OnKeyframe func(wait time.Duration)
}

// streamIDs are the streamId values of video1..video3 (see ChangeVideoSettings).
var streamIDs = [3]int{1, 2, 4}

func NewConsumer(opts StreamOptions) *Consumer {
	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionSendonly,
			Codecs:    []*core.Codec{{Name: core.CodecH264}, {Name: core.CodecH265}},
		},
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs:    []*core.Codec{{Name: core.CodecAAC}},
		},
	}
	return &Consumer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "unifi",
			Protocol:   "tcp",
			Medias:     medias,
		},
		opts: opts,
		done: make(chan struct{}),
		now:  time.Now,
	}
}

func (c *Consumer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, track.Codec)

	switch track.Codec.Name {
	case core.CodecH264:
		c.mu.Lock()
		c.video = track.Codec
		c.mu.Unlock()

		sender.Handler = c.handleVideo
		if track.Codec.IsRTP() {
			sender.Handler = h264.RTPDepay(track.Codec, sender.Handler)
		} else {
			sender.Handler = h264.RepairAVCC(track.Codec, sender.Handler)
		}

	case core.CodecH265:
		c.mu.Lock()
		c.video, c.hevc = track.Codec, true
		c.mu.Unlock()

		sender.Handler = c.handleVideo
		if track.Codec.IsRTP() {
			sender.Handler = h265.RTPDepay(track.Codec, sender.Handler)
		} else {
			sender.Handler = h265.RepairAVCC(track.Codec, sender.Handler)
		}

	case core.CodecAAC:
		c.mu.Lock()
		c.audio = track.Codec
		c.mu.Unlock()

		sender.Handler = c.handleAudio
		if track.Codec.IsRTP() {
			sender.Handler = aac.RTPDepay(sender.Handler)
		}

	default:
		return errors.New("unifi: unsupported codec " + track.Codec.Name)
	}

	sender.HandleRTP(track)
	c.Senders = append(c.Senders, sender)
	return nil
}

// TrackCodecs returns the video and audio codec the stream delivered (either may be nil).
func (c *Consumer) TrackCodecs() (video, audio *core.Codec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.video, c.audio
}

// KeyframeWanted notes that Protect asked for a keyframe, for OnKeyframe.
func (c *Consumer) KeyframeWanted() {
	c.mu.Lock()
	if c.keyWanted.IsZero() {
		c.keyWanted = c.now()
	}
	c.mu.Unlock()
}

// CountFrames waits d (or until ctx ends) and returns the video frames
// received meanwhile.
func (c *Consumer) CountFrames(ctx context.Context, d time.Duration) int {
	c.mu.Lock()
	start := c.frames
	c.mu.Unlock()
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames - start
}

// Size returns the video resolution, from the codec or the first keyframe (0 if unknown yet).
func (c *Consumer) Size() (width, height int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.width, c.height
}

// WriteTo starts streaming to wr and blocks until a write fails or Stop is called.
// Output begins with the first keyframe; earlier frames are dropped.
func (c *Consumer) WriteTo(wr io.Writer) (int64, error) {
	if closer, ok := wr.(io.Closer); ok {
		c.closer.Store(closer)
	}

	c.mu.Lock()
	c.wr = wr
	c.mu.Unlock()

	<-c.done

	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(c.Send), c.err
}

// OnStop registers fn to run once when the consumer is stopped (to release
// its source).
func (c *Consumer) OnStop(fn func()) {
	c.mu.Lock()
	if !c.onStopRan {
		c.onStop = append(c.onStop, fn)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	fn() // already stopped: release right away
}

// Stop ends WriteTo. The writer is closed first: a write blocked on a stalled
// socket holds c.mu and would otherwise delay Stop by the write timeout.
func (c *Consumer) Stop() error {
	if closer, ok := c.closer.Load().(io.Closer); ok {
		_ = closer.Close()
	}
	c.mu.Lock()
	c.finish(nil)
	c.mu.Unlock()
	err := c.Connection.Stop()
	c.stopped.Do(func() {
		c.mu.Lock()
		onStop := c.onStop
		c.onStop, c.onStopRan = nil, true
		c.mu.Unlock()
		for _, fn := range onStop {
			fn()
		}
	})
	return err
}

// finish must be called with c.mu held.
func (c *Consumer) finish(err error) {
	if c.closed {
		return
	}
	c.closed = true
	c.err = err
	close(c.done)
}

func (c *Consumer) handleVideo(pkt *rtp.Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	c.frames++
	if len(pkt.Payload) < 5 {
		return
	}
	key := c.isKeyframe(pkt.Payload)
	if key && c.width == 0 {
		c.width, c.height = c.size(pkt.Payload)
	}
	if key && !c.keyWanted.IsZero() {
		if c.opts.OnKeyframe != nil {
			c.opts.OnKeyframe(c.now().Sub(c.keyWanted))
		}
		c.keyWanted = time.Time{}
	}

	if c.wr == nil {
		return
	}

	now := c.now()

	b := c.buf[:0]
	if !c.started {
		if !key {
			return
		}
		c.started = true
		c.mux = newMuxer(now)
		if c.audio != nil {
			c.mux.audioClock = int64(clockRate(c.audio, 16000))
		}
		c.vclock = trackClock{rate: int64(clockRate(c.video, clockVideo))}
		b = c.appendInit(b, pkt.Payload)
	}

	at := c.vclock.next(pkt.Timestamp, now.Sub(c.mux.start))
	c.mux.stamp = now
	c.vpos = at

	payload := pkt.Payload
	if key {
		if !c.hevc {
			payload = c.replaceSPS(payload)
		}
		b = c.mux.appendSync(b, at, now)
	}

	// inter frame or keyframe, NALU; composition time stays 0
	hdr := [5]byte{frameInter<<4 | c.codecID(), 1}
	if key {
		hdr[0] = frameKey<<4 | c.codecID()
	}

	c.write(c.mux.appendTag(b, tagVideo, at, hdr[:], payload))
}

func (c *Consumer) handleAudio(pkt *rtp.Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Real cameras drop audio that arrives before the video timeline starts.
	if c.wr == nil || c.closed || !c.started || c.audio == nil {
		return
	}

	now := c.now()
	if c.aclock.rate == 0 {
		c.aclock = trackClock{rate: int64(clockRate(c.audio, 16000))}
	}
	at := c.aclock.next(pkt.Timestamp, now.Sub(c.mux.start))
	c.mux.stamp = now

	hdr := [2]byte{aacFlags, 1} // raw AAC frame
	c.write(c.mux.appendTag(c.buf[:0], tagAudio, at, hdr[:], pkt.Payload))
}

// aacFlags is the FLV audio header byte ffmpeg writes for AAC regardless of the
// real rate: AAC, "44 kHz", 16 bit, "stereo". Decoders take the real values
// from the AudioSpecificConfig.
const aacFlags = 10<<4 | 3<<2 | 1<<1 | 1

// write sends b, which is then kept as the next output buffer (Write does not
// retain it). Must be called with c.mu held.
func (c *Consumer) write(b []byte) {
	if cap(b) <= maxBuffer {
		c.buf = b[:0]
	}
	if d, ok := c.wr.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	n, err := c.wr.Write(b)
	c.Send += n
	if err != nil {
		c.finish(err)
	}
}

// appendInit appends the file header, onMetaData and the decoder configs. It is
// called with the first keyframe, so missing parameter sets can be taken from it.
func (c *Consumer) appendInit(b []byte, keyframe []byte) []byte {
	// Without an AudioSpecificConfig the AAC frames can't be decoded: send video only.
	var audioConfig []byte
	if c.audio != nil {
		audioConfig, _ = hex.DecodeString(core.Between(c.audio.FmtpLine, "config=", ";"))
		if len(audioConfig) == 0 {
			c.audio = nil
		}
	}

	b = append(b, fileHeader(c.audio != nil)...)
	b = c.mux.appendTag(b, tagScript, 0, scriptData("onMetaData", c.metadata(c.width, c.height)))

	if c.hevc {
		if vps, sps, pps := c.hevcParameterSets(keyframe); len(vps) > 0 && len(sps) > 0 && len(pps) > 0 {
			b = c.mux.appendTag(b, tagVideo, 0, hevcConfig(vps, sps, pps))
		}
	} else if sps, pps := c.parameterSets(keyframe); len(sps) > 0 && len(pps) > 0 {
		config := append([]byte{frameKey<<4 | codecAVC, 0, 0, 0, 0}, h264.EncodeConfig(c.fixSPS(sps), pps)...)
		b = c.mux.appendTag(b, tagVideo, 0, config)
	}

	if len(audioConfig) > 0 {
		b = c.mux.appendTag(b, tagAudio, 0, append([]byte{aacFlags, 0}, audioConfig...))
	}
	if c.opus {
		b = c.mux.appendTag(b, tagOpus, 0, opusConfig)
	}

	return b
}

// EnableOpus makes the stream carry Opus audio (WriteOpus) next to AAC. It
// must be called before the first keyframe, which carries the Opus config.
func (c *Consumer) EnableOpus() {
	c.mu.Lock()
	c.opus = true
	c.mu.Unlock()
}

// WriteOpus sends one Opus packet. pos is its position in 48 kHz samples
// (continuous, any start), captured the wall-clock time its sound was
// recorded (the source audio's arrival), so it lines up with the AAC track
// although the encoder delays it.
func (c *Consumer) WriteOpus(pkt []byte, pos uint32, captured time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wr == nil || c.closed || !c.started || !c.opus || len(pkt) == 0 {
		return
	}
	if c.oclock.rate == 0 {
		c.oclock = trackClock{rate: clockOpus}
	}
	at := c.oclock.next(pos, captured.Sub(c.mux.start))
	c.mux.stamp = c.now()
	c.write(c.mux.appendTag(c.buf[:0], tagOpus, at, pkt))
}

// fixSPS returns the SPS as sent to Protect: pixel format fixed and frame-rate
// timing added (see withTiming). The result is cached, since cameras repeat the
// same SPS with every keyframe.
func (c *Consumer) fixSPS(sps []byte) []byte {
	if c.spsIn != nil && bytes.Equal(sps, c.spsIn) {
		return c.spsOut
	}
	out := append([]byte(nil), sps...) // FixPixFmt writes in place; the payload is shared
	h264.FixPixFmt(out)
	out = withTiming(out, c.opts.FPS)
	c.spsIn, c.spsOut = append([]byte(nil), sps...), out
	return out
}

// replaceSPS returns an AVCC frame with its in-band SPS replaced by fixSPS's
// version, so the decoder config and the stream agree. The frame is shared with
// other consumers and is copied, never changed.
func (c *Consumer) replaceSPS(avcc []byte) []byte {
	var out []byte
	for i := 0; len(avcc)-i > 4; {
		size := int(binary.BigEndian.Uint32(avcc[i:]))
		if size <= 0 || i+4+size > len(avcc) {
			break
		}
		nalu := avcc[i+4 : i+4+size]
		if nalu[0]&0x1F == h264.NALUTypeSPS {
			if fixed := c.fixSPS(nalu); !bytes.Equal(fixed, nalu) {
				if out == nil {
					out = make([]byte, 0, len(avcc)+len(fixed)-len(nalu))
					out = append(out, avcc[:i]...)
				}
				out = binary.BigEndian.AppendUint32(out, uint32(len(fixed)))
				out = append(out, fixed...)
				i += 4 + size
				continue
			}
		}
		if out != nil {
			out = append(out, avcc[i:i+4+size]...)
		}
		i += 4 + size
	}
	if out == nil {
		return avcc
	}
	return out
}

// parameterSets returns SPS and PPS from the codec, or else from a keyframe.
func (c *Consumer) parameterSets(keyframe []byte) (sps, pps []byte) {
	sps, pps = h264.GetParameterSet(c.video.FmtpLine)
	if len(sps) == 0 || len(pps) == 0 {
		sps, pps = parameterSets(keyframe)
	}
	return
}

// trackClock turns a track's 32-bit RTP timestamps into a continuous position
// on the stream timeline, the way a camera stamps frames with its capture clock.
//
// 32-bit tick deltas wrap (after 13 h at 90 kHz) and restart when a go2rtc
// producer reconnects under a running consumer; on such a discontinuity the
// timeline is re-anchored to the wall-clock time since the stream started. The
// source's clock also drifts against the wall clock (crystals are off by tens
// of ppm, a second every few hours); beyond maxDrift the steps are nudged
// towards wall time. Positions never go backwards.
type trackClock struct {
	rate    int64 // ticks per second
	last    uint32
	ticks   int64 // position on the timeline, in ticks
	started bool
}

const (
	maxTrackGap = 5                      // seconds; a larger forward step is a discontinuity
	maxDrift    = 500 * time.Millisecond // tolerated distance from wall time
	driftSlew   = 256                    // fraction of the excess corrected per frame
)

func (t *trackClock) next(ts uint32, elapsed time.Duration) time.Duration {
	wall := ticks(elapsed, t.rate)
	if !t.started {
		t.started = true
		t.ticks = wall
	} else if d := int64(int32(ts - t.last)); d < 0 || d > maxTrackGap*t.rate {
		t.ticks = max(wall, t.ticks)
	} else {
		limit := ticks(maxDrift, t.rate)
		if diff := wall - (t.ticks + d); diff > limit {
			d += (diff - limit) / driftSlew
		} else if diff < -limit {
			d = max(d+(diff+limit)/driftSlew, 0)
		}
		t.ticks += d
	}
	t.last = ts
	return time.Duration(t.ticks/t.rate)*time.Second + time.Duration(t.ticks%t.rate*int64(time.Second)/t.rate)
}

// metadata returns onMetaData. By default it is the AMF object a real UniFi
// camera sends (libubnt_encoder in firmware 5.5.45; the keys as measured by
// pyunifiwire on a G5, alphabetical as the camera serialises them). Protect files
// recordings as <MAC>_<channelId>. The ffmpeg variant is what unifi-cam-proxy feeds
// Protect for H.264 channels.
func (c *Consumer) metadata(width, height int) any {
	if c.opts.FFmpegMetadata {
		return c.ffmpegMetadata(width, height)
	}

	ch := min(max(c.opts.Channel, 0), 2)
	var m amfObject
	if c.audio != nil {
		// the camera removes the audio keys from a stream without audio
		m = amfObject{
			{"audioBandwidth", 32000},
			{"audioChannels", max(int(c.audio.Channels), 1)},
			{"audioFrequency", int(c.audio.ClockRate)},
		}
	}
	return append(m, amfObject{
		{"channelId", ch},
		{"extendedFormat", true}, // extendedFlv serializer, as Protect requests
		{"hasAudio", c.audio != nil},
		{"hasVideo", true},
		{"streamId", streamIDs[ch]},
		{"streamName", c.opts.StreamName},
		{"videoBandwidth", c.opts.Bitrate},
		{"videoFps", c.opts.FPS},
		{"videoHeight", height},
		{"videoWidth", width},
	}...)
}

func (c *Consumer) ffmpegMetadata(width, height int) amfArray {
	m := amfArray{
		{"duration", 0.0},
		{"width", width},
		{"height", height},
		{"videodatarate", 0.0},
	}
	if c.opts.FPS > 0 {
		m = append(m, amfProp{"framerate", c.opts.FPS})
	}
	m = append(m, amfProp{"videocodecid", int(c.codecID())})

	if c.audio != nil {
		m = append(m,
			amfProp{"audiodatarate", 0.0},
			amfProp{"audiosamplerate", int(c.audio.ClockRate)},
			amfProp{"audiosamplesize", 16},
			amfProp{"stereo", c.audio.Channels == 2},
			amfProp{"audiocodecid", 10},
		)
	}

	return append(m,
		amfProp{"streamName", c.opts.StreamName},
		amfProp{"encoder", "Lavf61.7.100"},
		amfProp{"filesize", 0.0},
	)
}

func clockRate(codec *core.Codec, def uint32) uint32 {
	if codec != nil && codec.ClockRate >= 1000 {
		return codec.ClockRate
	}
	return def
}

// parameterSets extracts SPS and PPS from an AVCC (4-byte length prefixed) frame.
func parameterSets(avcc []byte) (sps, pps []byte) {
	for len(avcc) > 4 {
		size := int(avcc[0])<<24 | int(avcc[1])<<16 | int(avcc[2])<<8 | int(avcc[3])
		if size <= 0 || 4+size > len(avcc) {
			break
		}
		nalu := avcc[4 : 4+size]
		switch nalu[0] & 0x1F {
		case h264.NALUTypeSPS:
			sps = nalu
		case h264.NALUTypePPS:
			pps = nalu
		}
		avcc = avcc[4+size:]
	}
	return
}

// FLV video tag values. UniFi cameras carry H.265 under codec id 8 in
// legacy-shaped tags, not as Enhanced-RTMP "hvc1" tags.
const (
	frameKey      = 1
	frameInter    = 2
	frameHEVCConf = 6 // the H.265 decoder config tag
	codecAVC      = 7
	codecHEVC     = 8
)

func (c *Consumer) codecID() byte {
	if c.hevc {
		return codecHEVC
	}
	return codecAVC
}

func (c *Consumer) isKeyframe(avcc []byte) bool {
	if c.hevc {
		return h265.IsKeyframe(avcc)
	}
	return h264.IsKeyframe(avcc)
}

// size reads the resolution from the SPS of the codec or a keyframe.
func (c *Consumer) size(keyframe []byte) (width, height int) {
	if c.hevc {
		_, sps, _ := c.hevcParameterSets(keyframe)
		if s := h265.DecodeSPS(sps); s != nil {
			return int(s.Width()), int(s.Height())
		}
		return 0, 0
	}
	sps, _ := c.parameterSets(keyframe)
	if s := h264.DecodeSPS(sps); s != nil {
		return int(s.Width()), int(s.Height())
	}
	return 0, 0
}

// hevcParameterSets returns VPS, SPS and PPS from the codec, or else from a keyframe.
func (c *Consumer) hevcParameterSets(keyframe []byte) (vps, sps, pps []byte) {
	vps, sps, pps = h265.GetParameterSet(c.video.FmtpLine)
	if len(vps) == 0 || len(sps) == 0 || len(pps) == 0 {
		vps, sps, pps = hevcParameterSets(keyframe)
	}
	return
}

// hevcConfig is the H.265 decoder config as UniFi cameras send it: frame type
// 6, then the bare VPS, SPS and PPS with 2-byte lengths. A standard hvcC
// record here makes Protect treat the stream as an unknown codec (measured by
// rjmotion/pyunifiwire on real cameras, see reolink-unifi-protect).
func hevcConfig(vps, sps, pps []byte) []byte {
	b := []byte{frameHEVCConf<<4 | codecHEVC, 1}
	for _, nalu := range [][]byte{vps, sps, pps} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(nalu)))
		b = append(b, nalu...)
	}
	return b
}

// hevcParameterSets extracts VPS, SPS and PPS from an H.265 AVCC frame.
func hevcParameterSets(avcc []byte) (vps, sps, pps []byte) {
	for len(avcc) > 4 {
		size := int(binary.BigEndian.Uint32(avcc))
		if size <= 0 || 4+size > len(avcc) {
			break
		}
		nalu := avcc[4 : 4+size]
		switch (nalu[0] >> 1) & 0x3F {
		case h265.NALUTypeVPS:
			vps = nalu
		case h265.NALUTypeSPS:
			sps = nalu
		case h265.NALUTypePPS:
			pps = nalu
		}
		avcc = avcc[4+size:]
	}
	return
}

// RTT answers Protect's round-trip probe (feedback type 4) the way a UniFi
// camera does (libubnt_encoder ContainerFLV::FeedRtt/BuildOnRtt): an onRtt
// script tag right away, with the probe's id echoed as 4 raw bytes,
// rttMillis = time since the trailer Protect echoes (id(4) rate(4) ticks(8))
// was written, and encDelay = capture-to-encoder latency, 0 here since
// nothing is encoded. A camera computes it from its capture clock; trailers
// here follow the source's clock, which may be off wall time by up to
// maxDrift, so the send time of each recent trailer is kept instead. An
// unknown trailer gives -1. Nothing is sent before the stream has started.
func (c *Consumer) RTT(body []byte) (rttMillis float64, sent bool) {
	if len(body) < 16 {
		return 0, false
	}
	rate := int64(binary.BigEndian.Uint32(body[4:8]))
	echoed := binary.BigEndian.Uint64(body[8:16])
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wr == nil || c.closed || !c.started {
		return 0, false
	}
	now := c.now()
	rtt := -1.0
	if echoed <= math.MaxInt64 {
		if at, ok := c.mux.sentAt(rate, int64(echoed)); ok && !now.Before(at) {
			rtt = float64(now.Sub(at).Round(time.Millisecond).Milliseconds())
		}
	}
	msg := amfObject{
		{"encDelay", 0.0},
		{"id", string(body[0:4])},
		{"rttMillis", rtt},
	}
	c.mux.stamp = now
	c.write(c.mux.appendTag(c.buf[:0], tagScript, c.mux.last, scriptData("onRtt", msg)))
	return rtt, true
}
