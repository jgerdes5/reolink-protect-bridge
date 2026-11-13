package unifi

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/flv/amf"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// 2560x1920 SPS (same resolution as the Reolink doorbell main stream)
const testSPS = "Z0IAMukAUAHjQgAAB9IAAOqcCAA="

func avcc(nalus ...[]byte) []byte {
	var b []byte
	for _, n := range nalus {
		b = binary.BigEndian.AppendUint32(b, uint32(len(n)))
		b = append(b, n...)
	}
	return b
}

func TestConsumerOutput(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	video := &core.Codec{
		Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW,
		FmtpLine: "packetization-mode=1;sprop-parameter-sets=" +
			base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps),
	}
	audio := aac.ConfigToCodec(aac.EncodeConfig(aac.TypeAACLC, 16000, 1, false))

	c := NewConsumer(StreamOptions{StreamName: "Abc123_0", FPS: 20, Bitrate: 6000000})
	c.video, c.audio = video, audio
	clock := time.Unix(1700000000, 0)
	c.now = func() time.Time { return clock }

	out := &syncBuffer{}
	go func() { _, _ = c.WriteTo(out) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)

	pframe := avcc([]byte{0x41, 0x9a, 0x00})
	iframe := avcc([]byte{0x65, 0x88, 0x84, 0x00})

	c.handleAudio(&rtp.Packet{Header: rtp.Header{Timestamp: 100}, Payload: []byte{0xde, 0xad}}) // before keyframe: dropped
	c.handleVideo(&rtp.Packet{Header: rtp.Header{Timestamp: 1000}, Payload: pframe})            // before keyframe: dropped
	c.handleVideo(&rtp.Packet{Header: rtp.Header{Timestamp: 9000}, Payload: iframe})
	clock = clock.Add(40 * time.Millisecond)
	c.handleAudio(&rtp.Packet{Header: rtp.Header{Timestamp: 3200}, Payload: []byte{0xbe, 0xef}})
	c.handleAudio(&rtp.Packet{Header: rtp.Header{Timestamp: 3200 + 1024}, Payload: []byte{0xca, 0xfe}})
	c.handleVideo(&rtp.Packet{Header: rtp.Header{Timestamp: 9000 + 4500}, Payload: pframe})

	b := out.Bytes()
	require.Equal(t, fileHeader(true), b[:13])
	tags := parseUniFi(t, b[13:])
	require.Len(t, tags, 9)

	// onMetaData: the AMF object a real camera sends (0x03, not an ECMA array)
	require.Equal(t, byte(tagScript), tags[0].typ)
	require.Equal(t, byte(0x03), tags[0].payload[3+len("onMetaData")])
	items, err := amf.NewReader(tags[0].payload).ReadItems()
	require.NoError(t, err)
	require.Equal(t, "onMetaData", items[0])
	meta := items[1].(map[string]any)
	require.Equal(t, "Abc123_0", meta["streamName"])
	require.Equal(t, float64(0), meta["channelId"])
	require.Equal(t, float64(1), meta["streamId"])
	require.Equal(t, true, meta["extendedFormat"])
	require.Equal(t, true, meta["hasAudio"])
	require.Equal(t, true, meta["hasVideo"])
	require.Equal(t, float64(2560), meta["videoWidth"])
	require.Equal(t, float64(1920), meta["videoHeight"])
	require.Equal(t, float64(20), meta["videoFps"])
	require.Equal(t, float64(6000000), meta["videoBandwidth"])
	require.Equal(t, float64(16000), meta["audioFrequency"])
	require.Equal(t, float64(1), meta["audioChannels"])

	// AVC sequence header, then AAC sequence header
	require.Equal(t, byte(tagVideo), tags[1].typ)
	require.Equal(t, []byte{0x17, 0, 0, 0, 0}, tags[1].payload[:5])
	require.Equal(t, byte(tagAudio), tags[2].typ)
	require.Equal(t, []byte{0xAF, 0}, tags[2].payload[:2])

	// onMpma + onClockSync right before the keyframe, as a camera sends them
	require.Equal(t, "onMpma", scriptName(tags[3].payload))
	require.Equal(t, "onClockSync", scriptName(tags[4].payload))

	// keyframe at ts 0
	require.Equal(t, byte(tagVideo), tags[5].typ)
	require.Equal(t, uint32(0), tags[5].ts)
	require.Equal(t, append([]byte{0x17, 1, 0, 0, 0}, iframe...), tags[5].payload)
	require.Equal(t, "00015f90", hexOf(tags[5].trailer[:4]))

	// audio on the video timeline: first frame arrived 40 ms after the
	// keyframe, the next one 1024 samples (64 ms) later; trailers carry the
	// same times as the tags
	require.Equal(t, byte(tagAudio), tags[6].typ)
	require.Equal(t, uint32(40), tags[6].ts)
	require.Equal(t, []byte{0xAF, 1, 0xbe, 0xef}, tags[6].payload)
	require.Equal(t, uint32(104), tags[7].ts)
	require.Equal(t, uint64(40*clockOther/1000), binary.BigEndian.Uint64(tags[6].trailer[8:]))
	require.Equal(t, uint64(104*clockOther/1000), binary.BigEndian.Uint64(tags[7].trailer[8:]))

	// inter frame 4500 ticks = 50 ms later, trailer from the stream clock, not
	// from the arrival time (40 ms)
	require.Equal(t, byte(tagVideo), tags[8].typ)
	require.Equal(t, uint32(50), tags[8].ts)
	require.Equal(t, byte(0x27), tags[8].payload[0])
	require.Equal(t, uint64(4500), binary.BigEndian.Uint64(tags[8].trailer[8:]))

	require.NoError(t, c.Stop())
}

func TestConsumerParameterSetsFromKeyframe(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	c := NewConsumer(StreamOptions{StreamName: "s", Channel: 2, FFmpegMetadata: true})
	c.video = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	out := &syncBuffer{}
	go func() { _, _ = c.WriteTo(out) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)

	c.handleVideo(&rtp.Packet{Payload: avcc(sps, pps, []byte{0x65, 0x88})})

	b := out.Bytes()
	require.Equal(t, fileHeader(false), b[:13])
	tags := parseUniFi(t, b[13:])
	require.Len(t, tags, 5) // metadata, AVC config, onMpma, onClockSync, keyframe (no audio)
	items, err := amf.NewReader(tags[0].payload).ReadItems()
	require.NoError(t, err)
	require.Equal(t, byte(0x08), tags[0].payload[3+len("onMetaData")]) // ffmpeg: ECMA array
	meta := items[1].(map[string]any)
	require.Equal(t, float64(2560), meta["width"])
	require.Equal(t, "s", meta["streamName"])
	require.NotContains(t, meta, "framerate")
	require.NotContains(t, meta, "audiocodecid")
	require.Equal(t, []byte{0x17, 0, 0, 0, 0}, tags[1].payload[:5])
}

func scriptName(payload []byte) string {
	items, _ := amf.NewReader(payload).ReadItems()
	if len(items) == 0 {
		return ""
	}
	name, _ := items[0].(string)
	return name
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	s := make([]byte, 0, len(b)*2)
	for _, v := range b {
		s = append(s, digits[v>>4], digits[v&15])
	}
	return string(s)
}

func TestConsumerMetadataChannels(t *testing.T) {
	for ch, streamID := range []float64{1, 2, 4} {
		c := NewConsumer(StreamOptions{StreamName: "x", Channel: ch})
		meta := c.metadata(640, 480).(Object)
		got := map[string]any{}
		for _, p := range meta {
			got[p.Key] = p.Value
		}
		require.Equal(t, ch, got["channelId"])
		require.Equal(t, int(streamID), got["streamId"])
		require.Equal(t, false, got["hasAudio"])
		require.NotContains(t, got, "audioFrequency") // removed without audio, as on the camera
		// keys in the camera's alphabetical order
		for i := 1; i < len(meta); i++ {
			require.Less(t, meta[i-1].Key, meta[i].Key)
		}
	}
}

func TestTrackClock(t *testing.T) {
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	ms := func(d time.Duration) int64 { return d.Milliseconds() }

	c := trackClock{rate: 90000}
	base := uint32(0xFFFFFFFF - 90000) // 1 s before the 32-bit RTP clock wraps
	require.Equal(t, int64(0), ms(c.next(base, 0)))
	require.Equal(t, int64(1000), ms(c.next(base+90000, sec(1))))
	require.Equal(t, int64(2000), ms(c.next(base+180000, sec(2)))) // across the wrap: continuous

	// the source reconnected and its timestamps restarted: re-anchor to wall time
	require.Equal(t, int64(5000), ms(c.next(123, sec(5))))
	require.Equal(t, int64(5040), ms(c.next(123+3600, sec(5.04))))

	// a forward jump of more than 5 s is a discontinuity too
	require.Equal(t, int64(6000), ms(c.next(123+3600+90000*600, sec(6))))

	// a restart re-anchors, but never to before the current position
	require.Equal(t, int64(6000), ms(c.next(5, sec(5.9))))
}

func TestTrackClockNonIntegerRate(t *testing.T) {
	// 44.1 kHz: a rate/1000 divisor (44) drifted 8 s per hour
	c := trackClock{rate: 44100}
	var ts uint32
	var at time.Duration
	frames := 3600 * 44100 / 1024
	for i := 0; i < frames; i++ {
		at = c.next(ts, time.Duration(int64(ts)*int64(time.Second)/44100))
		ts += 1024
	}
	exact := time.Duration(int64(frames-1) * 1024 * int64(time.Second) / 44100)
	require.InDelta(t, float64(exact), float64(at), float64(time.Millisecond))
}

// A source clock running 200 ppm fast or slow (0.7 s per hour) is held within
// maxDrift of wall time, without the timeline ever going backwards.
func TestTrackClockDrift(t *testing.T) {
	for _, ppm := range []float64{200, -200} {
		c := trackClock{rate: 90000}
		var ts uint32
		var prev time.Duration
		const fps = 20
		for i := 0; i < 10*3600*fps; i++ { // 10 h
			wall := time.Duration(i) * time.Second / fps
			at := c.next(ts, wall)
			require.GreaterOrEqual(t, at, prev)
			prev = at
			if i > 3600*fps {
				require.InDelta(t, float64(wall), float64(at), float64(maxDrift+50*time.Millisecond), "ppm %v", ppm)
			}
			ts += uint32(90000 / fps * (1 + ppm/1e6))
		}
	}
}

// blockingConn's Write blocks until Close, like a socket to a stalled peer.
type blockingConn struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingConn) Write(p []byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingConn) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestConsumerStopDuringStalledWrite(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	c := NewConsumer(StreamOptions{StreamName: "s"})
	c.video = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}

	conn := &blockingConn{closed: make(chan struct{})}
	wrote := make(chan error, 1)
	go func() { _, err := c.WriteTo(conn); wrote <- err }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)

	go c.handleVideo(&rtp.Packet{Payload: avcc(sps, []byte{0x68, 0xce, 0x38, 0x80}, []byte{0x65, 0x88})})
	time.Sleep(50 * time.Millisecond) // the handler is now blocked in Write, holding c.mu

	start := time.Now()
	require.NoError(t, c.Stop())
	require.Less(t, time.Since(start), time.Second)
	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("WriteTo did not return")
	}
}

func TestConsumerAudioWithoutConfig(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	c := NewConsumer(StreamOptions{StreamName: "s"})
	c.video = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	c.audio = &core.Codec{Name: core.CodecAAC, ClockRate: 16000} // no config=

	out := &syncBuffer{}
	go func() { _, _ = c.WriteTo(out) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)

	c.handleVideo(&rtp.Packet{Payload: avcc(sps, []byte{0x68, 0xce, 0x38, 0x80}, []byte{0x65, 0x88})})
	c.handleAudio(&rtp.Packet{Payload: []byte{1, 2}})

	tags := parseUniFi(t, out.Bytes()[13:])
	require.Len(t, tags, 5) // metadata, AVC config, onMpma, onClockSync, keyframe: no undecodable audio
	items, err := amf.NewReader(tags[0].payload).ReadItems()
	require.NoError(t, err)
	require.Equal(t, false, items[1].(map[string]any)["hasAudio"])
	for _, tag := range tags {
		require.NotEqual(t, byte(tagAudio), tag.typ)
	}
	require.NoError(t, c.Stop())
}

// The RTP path (e.g. an rtsp:// source): packets go through AddTrack, the
// sender and h264.RTPDepay, which adds SPS/PPS to keyframes.
func TestConsumerRTPSource(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	codec := &core.Codec{
		Name: core.CodecH264, ClockRate: 90000, PayloadType: 96,
		FmtpLine: "packetization-mode=1;sprop-parameter-sets=" +
			base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps),
	}
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}}
	receiver := core.NewReceiver(media, codec)

	c := NewConsumer(StreamOptions{StreamName: "rtp_0"})
	require.NoError(t, c.AddTrack(media, codec, receiver))

	out := &syncBuffer{}
	go func() { _, _ = c.WriteTo(out) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)

	receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Marker: true, SequenceNumber: 1, Timestamp: 90000}, Payload: []byte{0x41, 0x9a}}) // P: dropped
	receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Marker: true, SequenceNumber: 2, Timestamp: 93000}, Payload: []byte{0x65, 0x88, 0x84}})
	receiver.WriteRTP(&rtp.Packet{Header: rtp.Header{Marker: true, SequenceNumber: 3, Timestamp: 97500}, Payload: []byte{0x41, 0x9a, 0x01}})

	require.Eventually(t, func() bool {
		b := out.Bytes()
		return len(b) > 13 && len(parseUniFi(t, b[13:])) == 6
	}, time.Second, 5*time.Millisecond)

	// metadata, AVC config, onMpma, onClockSync, keyframe, inter frame
	tags := parseUniFi(t, out.Bytes()[13:])
	require.Equal(t, byte(0x17), tags[4].payload[0]) // keyframe first
	require.Equal(t, uint32(0), tags[4].ts)
	require.Equal(t, byte(0x27), tags[5].payload[0])
	require.Equal(t, uint32(50), tags[5].ts) // 4500 ticks
	w, h := c.Size()
	require.Equal(t, 2560, w)
	require.Equal(t, 1920, h)
	require.NoError(t, c.Stop())
}

// BenchmarkConsumerVideo measures one 300 KB keyframe-sized frame through
// handleVideo (allocations should stay near zero with the reused buffer).
func BenchmarkConsumerVideo(b *testing.B) {
	c := NewConsumer(StreamOptions{StreamName: "s", FPS: 20})
	c.video = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	c.wr = io.Discard
	sps, _ := base64.StdEncoding.DecodeString(doorbellSPS)
	c.handleVideo(&rtp.Packet{Payload: avcc(sps, []byte{0x68, 0xce, 0x38, 0x80}, []byte{0x65, 0x88})})

	frame := avcc(append([]byte{0x41}, make([]byte, 300_000)...))
	pkt := &rtp.Packet{Payload: frame}
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for i := 0; i < b.N; i++ {
		pkt.Timestamp += 4500
		c.handleVideo(pkt)
	}
}

// TestConsumerHEVC checks the UniFi H.265 framing: codec id 8, a frame type 6
// config with bare 2-byte-length VPS/SPS/PPS, frames as 4-byte NAL units, and
// the in-band parameter sets left untouched.
func TestConsumerHEVC(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString("QgEBAWAAAAMAAAMAAAMAAAMAmaAAoAgBaH+KrTuiS7/8AAQABbAgApMuADN/mAE=") // 5120x1440
	vps := []byte{0x40, 0x01, 0x0c, 0x01}
	pps := []byte{0x44, 0x01, 0xc1, 0x72}
	idr := []byte{0x26, 0x01, 0xaf}    // IDR_W_RADL
	pslice := []byte{0x02, 0x01, 0xd0} // TRAIL_R

	c := NewConsumer(StreamOptions{StreamName: "s", Channel: 0, FPS: 15})
	recv := core.NewReceiver(c.Medias[0], &core.Codec{Name: core.CodecH265, ClockRate: 90000, PayloadType: core.PayloadTypeRAW})
	require.NoError(t, c.AddTrack(c.Medias[0], nil, recv))
	out := &syncBuffer{}
	go func() { _, _ = c.WriteTo(out) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)

	key := avcc(vps, sps, pps, idr)
	recv.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: avcc(pslice)}) // before the first keyframe: dropped
	recv.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: 6000}, Payload: key})
	recv.WriteRTP(&rtp.Packet{Header: rtp.Header{Timestamp: 12000}, Payload: avcc(pslice)})
	require.Eventually(t, func() bool { return len(out.Bytes()) > 13 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	tags := parseUniFi(t, out.Bytes()[13:])
	require.Len(t, tags, 6) // metadata, HEVC config, onMpma, onClockSync, keyframe, inter frame
	w, h := c.Size()
	require.Equal(t, [2]int{5120, 1440}, [2]int{w, h})

	config := []byte{0x68, 1}
	for _, n := range [][]byte{vps, sps, pps} {
		config = binary.BigEndian.AppendUint16(config, uint16(len(n)))
		config = append(config, n...)
	}
	require.Equal(t, config, tags[1].payload)
	require.Equal(t, append([]byte{0x18, 1, 0, 0, 0}, key...), tags[4].payload)
	require.Equal(t, append([]byte{0x28, 1, 0, 0, 0}, avcc(pslice)...), tags[5].payload)
}

// The onRtt reply: 62-byte script body as the camera builds it.
func TestConsumerRTT(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	c := NewConsumer(StreamOptions{StreamName: "s"})
	c.video = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	start := time.Unix(1000, 0)
	now := start
	c.now = func() time.Time { return now }
	out := &syncBuffer{}

	c.RTT(make([]byte, 16)) // before streaming: nothing
	go func() { _, _ = c.WriteTo(out) }()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.wr != nil
	}, time.Second, time.Millisecond)
	c.handleVideo(&rtp.Packet{Payload: avcc(sps, []byte{0x68, 0xce, 0x38, 0x80}, []byte{0x65, 0x88})})
	before := len(out.Bytes())

	// Protect echoes our trailer from 2 s into the stream; 2.25 s have passed
	now = start.Add(2250 * time.Millisecond)
	probe := []byte{'a', 'b', 'c', 'd', 0, 0, 0, 0} // id, then the rate
	binary.BigEndian.PutUint32(probe[4:], 90000)
	probe = binary.BigEndian.AppendUint64(probe, 2*90000)
	c.RTT(probe)

	tags := parseUniFi(t, out.Bytes()[before:])
	require.Len(t, tags, 1)
	require.Len(t, tags[0].payload, 62)
	items, err := amf.NewReader(tags[0].payload).ReadItems()
	require.NoError(t, err)
	require.Equal(t, "onRtt", items[0])
	m := items[1].(map[string]any)
	require.Equal(t, "abcd", m["id"])
	require.Equal(t, float64(250), m["rttMillis"])
	require.Equal(t, float64(0), m["encDelay"])

	// an echoed time ahead of our clock: -1
	binary.BigEndian.PutUint64(probe[8:], 10*90000)
	c.RTT(probe)
	tags = parseUniFi(t, out.Bytes()[before:])
	items, _ = amf.NewReader(tags[1].payload).ReadItems()
	require.Equal(t, float64(-1), items[1].(map[string]any)["rttMillis"])
}
