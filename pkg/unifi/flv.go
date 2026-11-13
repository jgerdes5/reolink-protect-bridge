package unifi

import (
	"encoding/binary"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/flv"
)

// UniFi cameras push FLV over a plain TCP socket that Protect hands out in
// ChangeVideoSettings ("extendedFlv"). It is ordinary FLV with three additions,
// as written by libubnt_encoder (ContainerFLV) in UniFi firmware 5.5.45:
//
//   - the header flags byte has bit 2 set for "extended format" on top of the
//     usual audio (4) and video (1) bits, so 7 for a stream with audio;
//   - every tag (after its PreviousTagSize) is followed by a 16-byte trailer:
//     a 32-bit clock rate (90000 for video tags, 11025 for all others), 32 zero
//     bits, and the tag's time as a 64-bit count of that clock's ticks. The
//     camera uses its capture clock, the same clock as the tag timestamp;
//   - before every video keyframe an onMpma tag (bitrate envelope) and an
//     onClockSync tag map the stream clock to wall-clock time.
//
// unifi-cam-proxy's clock_sync.py writes the same layout, but it sits behind
// ffmpeg in a pipe and sees neither capture times nor frame types: it stamps
// trailers with the time it reads a tag, truncates them to 32 bits (wrapping
// after 13 h at 90 kHz) and sends the sync tags on a 5 s timer.
const (
	tagAudio  = flv.TagAudio
	tagVideo  = flv.TagVideo
	tagScript = flv.TagData

	clockVideo = 90000
	clockOther = 11025
)

// fileHeader is the FLV header with UniFi's flags, plus PreviousTagSize0.
func fileHeader(audio bool) []byte {
	flags := byte(1 | 2) // video, extended format
	if audio {
		flags |= 4
	}
	return []byte{'F', 'L', 'V', 1, flags, 0, 0, 0, 9, 0, 0, 0, 0}
}

// muxer turns FLV tags into the UniFi byte stream. Positions ("at") are on the
// stream timeline, which starts at 0 with the first keyframe. It is not safe
// for concurrent use; the Consumer serialises calls.
type muxer struct {
	start time.Time // wall-clock time of the first keyframe
}

func newMuxer(now time.Time) *muxer {
	return &muxer{start: now}
}

// ticks converts a timeline position to ticks of clock without overflowing.
func ticks(at time.Duration, clock int64) int64 {
	return int64(at/time.Second)*clock + int64(at%time.Second)*clock/int64(time.Second)
}

func appendTrailer(b []byte, video bool, at time.Duration) []byte {
	clock := int64(clockOther)
	if video {
		clock = clockVideo
	}
	b = binary.BigEndian.AppendUint32(b, uint32(clock))
	b = append(b, 0, 0, 0, 0)
	return binary.BigEndian.AppendUint64(b, uint64(max(ticks(at, clock), 0)))
}

// flvTime is the 32-bit millisecond FLV timestamp of a timeline position.
func flvTime(at time.Duration) uint32 {
	return uint32(at.Milliseconds())
}

// appendTag appends one tag, its payload being the concatenated parts, with
// PreviousTagSize and trailer. Writing the parts straight into b copies a frame
// once instead of three times (media header, flv.EncodeTag, trailer append).
func (m *muxer) appendTag(b []byte, tagType byte, at time.Duration, parts ...[]byte) []byte {
	size := 0
	for _, p := range parts {
		size += len(p)
	}
	ts := flvTime(at)
	b = append(b, tagType, byte(size>>16), byte(size>>8), byte(size),
		byte(ts>>16), byte(ts>>8), byte(ts), byte(ts>>24), 0, 0, 0)
	for _, p := range parts {
		b = append(b, p...)
	}
	b = binary.BigEndian.AppendUint32(b, uint32(11+size))
	return appendTrailer(b, tagType == tagVideo, at)
}

// appendSync appends onMpma and onClockSync, which a real camera sends right
// before every keyframe. streamClock is the tag time, wallClock the wall-clock
// time in ms; streamClockBase stays 0 because the timeline (and so every
// trailer) starts at 0, which keeps trailer = streamClockBase + tag time as on
// a camera.
func (m *muxer) appendSync(b []byte, at time.Duration, now time.Time) []byte {
	b = m.appendTag(b, tagScript, at, ScriptData("onMpma", mpma))

	sync := Object{
		{"streamClock", int64(flvTime(at))},
		{"streamClockBase", 0},
		{"wallClock", float64(now.UnixMilli())},
	}
	return m.appendTag(b, tagScript, at, ScriptData("onClockSync", sync))
}

// mpma is the fixed onMpma body clock_sync.py sends (bitrate hints).
var mpma = Object{
	{"cs", Object{{"cur", 1500000}, {"max", 1500000}, {"min", 1500000}}},
	{"m", Object{{"cur", 1500000}, {"max", 1500000}, {"min", 1500000}}},
	{"r", 0},
	{"sp", Object{{"cur", 1500000}, {"max", 1500000}, {"min", 1500000}}},
	{"t", 75000.0},
}
