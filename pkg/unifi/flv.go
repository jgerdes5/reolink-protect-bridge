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
//     a 32-bit clock rate, 32 zero bits, and the tag's time as a 64-bit count
//     of that clock's ticks. The camera uses its capture clock, the same clock
//     as the tag timestamp. Firmware 5.5.45 used 90000 for video and 11025 for
//     everything else; a G4 Instant on Protect 7.3 (captured) uses 90000 for
//     video and script tags, the sample rate (16000) for AAC and 48000 for
//     Opus, which is what is written here;
//   - besides AAC audio (tag 8) it sends Opus (tag 10) when Protect asks for
//     it (avSerializer parameters withOpus): a config tag cf 00 03 02, then
//     one raw Opus packet (20 ms) per tag. Protect's live view (WebRTC) plays
//     Opus, so without it Protect has to convert the audio first;
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
	tagOpus   = 10 // UniFi extension

	clockVideo = 90000
	clockOpus  = 48000
)

// opusConfig is the payload of the Opus config tag, as a G4 Instant sends it.
// libubnt_encoder (ContainerFLV::BuildAudioOpusCodecBytes) writes cf 00 03 and
// the sample rate's index: 48000 0, 24000 1, 16000 2, 12000 3, 8000 4. The
// bridge encodes at 16 kHz.
var opusConfig = []byte{0xcf, 0x00, 0x03, 0x02}

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
	audioClock int64         // trailer clock of AAC tags: the sample rate
	mpma       amfObject     // onMpma body; nil = the fixed one
	start      time.Time     // wall-clock time of the first keyframe
	stamp      time.Time     // wall-clock time of the tags being built now
	last       time.Duration // latest position written, so tags never go back
	sent       [64]sentTag   // recent trailers, to answer RTT probes
	next       int
}

// sentTag is a trailer as written and when: Protect's RTT probe echoes one.
type sentTag struct {
	clock, ticks int64
	at           time.Time
}

// sentAt is when the trailer Protect echoes was written: the one with these
// ticks of this clock, or, for a probe in another unit (Protect also probes
// in milliseconds, clock 1000), the tag at that time, to within one tick of
// the coarser clock.
func (m *muxer) sentAt(clock, t int64) (time.Time, bool) {
	if clock <= 0 {
		return time.Time{}, false
	}
	for _, s := range m.sent {
		if s.clock == clock && s.ticks == t && !s.at.IsZero() {
			return s.at, true
		}
	}
	target := time.Duration(t/clock)*time.Second + time.Duration(t%clock*int64(time.Second)/clock)
	var best time.Time
	bestDiff := time.Duration(-1)
	for _, s := range m.sent {
		if s.at.IsZero() || s.clock <= 0 {
			continue
		}
		pos := time.Duration(s.ticks/s.clock)*time.Second + time.Duration(s.ticks%s.clock*int64(time.Second)/s.clock)
		diff := pos - target
		if diff < 0 {
			diff = -diff
		}
		tick := time.Second / time.Duration(min(clock, s.clock))
		if diff <= tick && (bestDiff < 0 || diff < bestDiff) {
			best, bestDiff = s.at, diff
		}
	}
	return best, bestDiff >= 0
}

func newMuxer(now time.Time) *muxer {
	return &muxer{start: now}
}

// ticks converts a timeline position to ticks of clock without overflowing.
func ticks(at time.Duration, clock int64) int64 {
	return int64(at/time.Second)*clock + int64(at%time.Second)*clock/int64(time.Second)
}

func appendTrailer(b []byte, clock int64, at time.Duration) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(clock))
	b = append(b, 0, 0, 0, 0)
	return binary.BigEndian.AppendUint64(b, uint64(max(ticks(at, clock), 0)))
}

// clock is a tag type's trailer clock.
func (m *muxer) clock(tagType byte) int64 {
	switch tagType {
	case tagAudio:
		if m.audioClock > 0 {
			return m.audioClock
		}
		return 16000
	case tagOpus:
		return clockOpus
	}
	return clockVideo
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
	clock := m.clock(tagType)
	m.sent[m.next] = sentTag{clock: clock, ticks: max(ticks(at, clock), 0), at: m.stamp}
	m.next = (m.next + 1) % len(m.sent)
	m.last = max(m.last, at)
	return appendTrailer(b, clock, at)
}

// appendSync appends onMpma and onClockSync, which a real camera sends right
// before every keyframe. streamClock is the tag time, wallClock the wall-clock
// time in ms; streamClockBase stays 0 because the timeline (and so every
// trailer) starts at 0, which keeps trailer = streamClockBase + tag time as on
// a camera.
func (m *muxer) appendSync(b []byte, at time.Duration, now time.Time) []byte {
	mp := m.mpma
	if mp == nil {
		mp = mpma
	}
	b = m.appendTag(b, tagScript, at, scriptData("onMpma", mp))

	sync := amfObject{
		{"streamClock", int64(flvTime(at))},
		{"streamClockBase", 0},
		{"wallClock", float64(now.UnixMilli())},
	}
	return m.appendTag(b, tagScript, at, scriptData("onClockSync", sync))
}

// mpmaFor is onMpma as a G4 Instant sends it (captured on Protect 7.3), for
// a stream configured to bitrate bit/s that can go down to minimum: cs is the
// configured cap (min 32000), m the encoder's adaptive target (cur) within
// its range, sp the server/player limit, t the current target. Protect plans
// live view and its bitrate requests (feedback type 0) from these.
func mpmaFor(bitrate, minimum int) amfObject {
	if minimum <= 0 || minimum > bitrate {
		minimum = bitrate / 5
	}
	return amfObject{
		{"cs", amfObject{{"cur", bitrate}, {"max", bitrate}, {"min", 32000}}},
		{"m", amfObject{{"cur", bitrate}, {"max", bitrate}, {"min", minimum}}},
		{"r", 0},
		{"sp", amfObject{{"cur", bitrate}, {"max", bitrate}, {"min", 0}}},
		{"t", float64(bitrate)},
	}
}

// mpma is the fixed onMpma body clock_sync.py sends (bitrate hints), used
// when the stream's bitrate is unknown.
var mpma = amfObject{
	{"cs", amfObject{{"cur", 1500000}, {"max", 1500000}, {"min", 1500000}}},
	{"m", amfObject{{"cur", 1500000}, {"max", 1500000}, {"min", 1500000}}},
	{"r", 0},
	{"sp", amfObject{{"cur", 1500000}, {"max", 1500000}, {"min", 1500000}}},
	{"t", 75000.0},
}
