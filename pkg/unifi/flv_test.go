package unifi

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func amfNum(n float64) string {
	b := binary.BigEndian.AppendUint64(nil, math.Float64bits(n))
	return "00" + hex.EncodeToString(b)
}

func amfKey(s string) string {
	return hex.EncodeToString([]byte{byte(len(s) >> 8), byte(len(s))}) + hex.EncodeToString([]byte(s))
}

// flvlib3 (used by clock_sync.py) encodes create_script_tag("onClockSync", FLVObject)
// as: string marker + name, object marker, key/number pairs, empty key + 0x09.
func TestScriptDataMatchesFlvlib(t *testing.T) {
	got := scriptData("onClockSync", amfObject{
		{"streamClock", int64(1234)},
		{"streamClockBase", 0},
		{"wallClock", 1.5},
	})
	want := "02" + amfKey("onClockSync") + "03" +
		amfKey("streamClock") + amfNum(1234) +
		amfKey("streamClockBase") + amfNum(0) +
		amfKey("wallClock") + amfNum(1.5) +
		"000009"
	require.Equal(t, want, hex.EncodeToString(got))
}

func TestScriptDataECMAArray(t *testing.T) {
	got := scriptData("onMetaData", amfArray{{"streamName", "abc"}, {"stereo", false}})
	want := "02" + amfKey("onMetaData") + "08" + "00000002" +
		amfKey("streamName") + "02" + amfKey("abc") +
		amfKey("stereo") + "0100" +
		"000009"
	require.Equal(t, want, hex.EncodeToString(got))
}

// The trailer as ContainerFLV::AddExtendedTimestamps writes it: 32-bit clock
// rate, 32 zero bits, 64-bit ticks of that clock.
func TestTrailer(t *testing.T) {
	video := appendTrailer(nil, true, 1500*time.Millisecond)
	require.Equal(t, "00015f90"+"00000000"+"0000000000020f58", hex.EncodeToString(video)) // 135000

	other := appendTrailer(nil, false, 2*time.Second)
	require.Equal(t, "00002b11"+"00000000"+"0000000000005622", hex.EncodeToString(other)) // 22050

	// 14 h at 90 kHz no longer fits 32 bits; clock_sync.py's uint32 wrapped here
	long := appendTrailer(nil, true, 14*time.Hour)
	require.Equal(t, uint64(14*3600*90000), binary.BigEndian.Uint64(long[8:]))

	// 30 days without overflowing the conversion
	require.Equal(t, int64(30*86400)*90000, ticks(30*24*time.Hour, 90000))
}

type flvTag struct {
	typ     byte
	ts      uint32
	payload []byte
	trailer []byte
}

// parseUniFi splits a UniFi FLV stream (after the 13-byte header) into tags.
func parseUniFi(t *testing.T, b []byte) []flvTag {
	var tags []flvTag
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 11)
		size := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		ts := uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6]) | uint32(b[7])<<24
		require.GreaterOrEqual(t, len(b), 11+size+4+16)
		require.Equal(t, uint32(11+size), binary.BigEndian.Uint32(b[11+size:]), "PreviousTagSize")
		tags = append(tags, flvTag{
			typ:     b[0],
			ts:      ts,
			payload: b[11 : 11+size],
			trailer: b[11+size+4 : 11+size+4+16],
		})
		b = b[11+size+4+16:]
	}
	return tags
}

func TestMuxerClockSync(t *testing.T) {
	now := time.Unix(1700000000, 123e6)
	m := newMuxer(now)

	// onMpma, then onClockSync, both at the keyframe's time with 11025 Hz trailers
	tags := parseUniFi(t, m.appendSync(nil, 5*time.Second, now))
	require.Len(t, tags, 2)

	require.Equal(t, byte(tagScript), tags[0].typ)
	require.Equal(t, uint32(5000), tags[0].ts)
	require.Equal(t, scriptData("onMpma", mpma), tags[0].payload)

	require.Equal(t, byte(tagScript), tags[1].typ)
	require.Equal(t, uint32(5000), tags[1].ts)
	require.Equal(t, hex.EncodeToString(scriptData("onClockSync", amfObject{
		{"streamClock", int64(5000)},
		{"streamClockBase", 0},
		{"wallClock", float64(1700000000123)},
	})), hex.EncodeToString(tags[1].payload))
	require.Equal(t, "00002b11", hex.EncodeToString(tags[1].trailer[:4]))
	require.Equal(t, uint64(5*clockOther), binary.BigEndian.Uint64(tags[1].trailer[8:]))

	// a video tag: 90 kHz trailer on the same timeline
	tags = parseUniFi(t, m.appendTag(nil, tagVideo, 5*time.Second, []byte{3}))
	require.Equal(t, "00015f90", hex.EncodeToString(tags[0].trailer[:4]))
	require.Equal(t, uint64(5*clockVideo), binary.BigEndian.Uint64(tags[0].trailer[8:]))
}

func TestFileHeaderFlags(t *testing.T) {
	require.Equal(t, byte(7), fileHeader(true)[4])  // video, extended, audio
	require.Equal(t, byte(3), fileHeader(false)[4]) // video, extended
}

func TestFirmwareVersion(t *testing.T) {
	for uri, want := range map[string]string{
		"https://192.168.1.1:7444/internal/update/sav837gw/uvc/firmware/5.5.31/AABBCCDDEEFF": "5.5.31",
		"https://fw-download.ubnt.com/data/uvc/e7fc-sav837gw-5.4.132-c960a165.bin":           "5.4.132",
		"https://fw-download.ubnt.com/data/uvc/efbc-sav837gw-5.5.45.bin":                     "5.5.45",
		"https://host/firmware/UVC.S5L.v5.5.45.67.0dda191.bin":                               "",
		"https://host/no/version/here":                                                       "",
	} {
		require.Equal(t, want, firmwareVersion(uri), uri)
	}
}
