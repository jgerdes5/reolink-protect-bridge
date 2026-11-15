package bridge

import (
	"testing"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/stretchr/testify/require"
)

// StreamInfoList as the doorbell builds it: an encodeTable per stream type and
// resolution.
const encCapsXML = `<body><StreamInfoList version="1.1"><StreamInfo><channelBits>1</channelBits>
<encodeTable><type>mainStream</type><resolution><width>1920</width><height>1440</height></resolution>
<defaultFramerate>15</defaultFramerate><defaultBitrate>3072</defaultBitrate>
<framerateTable>15,12,10</framerateTable><bitrateTable>1024,2048,3072</bitrateTable></encodeTable>
<encodeTable><type>mainStream</type><resolution><width>2560</width><height>1920</height></resolution>
<defaultFramerate>20</defaultFramerate><defaultBitrate>6144</defaultBitrate>
<framerateTable>20,18,16,15,12,10,8,6,4,2</framerateTable><bitrateTable>4096,5120,6144,7168,8192</bitrateTable></encodeTable>
<encodeTable><type>subStream</type><resolution><width>640</width><height>480</height></resolution>
<defaultFramerate>15</defaultFramerate><defaultBitrate>512</defaultBitrate>
<framerateTable>15,10,7,4</framerateTable><bitrateTable>128,256,384,512,768,1024</bitrateTable></encodeTable>
</StreamInfo></StreamInfoList></body>`

func TestEncoderState(t *testing.T) {
	enc := encoderState(parse(t, encXML), parse(t, encCapsXML))
	require.Equal(t, encStream{fps: 20, kbps: 6144, fpsTable: []int{20, 18, 16, 15, 12, 10, 8, 6, 4, 2}, kbpsTable: []int{4096, 5120, 6144, 7168, 8192}}, enc[0]) // the 2560x1920 table
	require.Equal(t, []int{128, 256, 384, 512, 768, 1024}, enc[1].kbpsTable)

	b := &Bridge{}
	b.setEncoder(enc)
	require.Equal(t, 20, b.info[0].fps) // the encoder's setting replaces the measured rate
	ch := unifi.Channel{}
	b.encoderChannel(&ch, 2) // video3 is the sub stream
	require.Equal(t, []int{4, 7, 10, 15}, ch.FPSValues)
	require.Equal(t, [3]int{512000, 128000, 1024000}, [3]int{ch.Bitrate, ch.BitrateMin, ch.BitrateMax})

	// without the list of choices: values only
	enc = encoderState(parse(t, encXML), nil)
	require.Nil(t, enc[0].fpsTable)
	require.Equal(t, 15, enc[1].fps)
}

func TestVideoChange(t *testing.T) {
	enc := encoderState(parse(t, encXML), parse(t, encCapsXML))

	body := parse(t, encXML)
	require.True(t, videoChange(map[string]unifi.Video{
		"video1": {Channel: "video1", FPS: 17, Bitrate: 5500000}, // -> 18 fps (higher on a tie), 5120 kbit/s
		"video3": {Channel: "video3", FPS: 8},                    // sub stream without video2 -> 7 fps
	}, enc)(body))
	require.Equal(t, "18", body.Value("Compression/mainStream/frame"))
	require.Equal(t, "5120", body.Value("Compression/mainStream/bitRate"))
	require.Equal(t, "18", body.Value("Compression/mainStream/gop/cur")) // keyframe every second
	require.Equal(t, "7", body.Value("Compression/subStream/frame"))
	require.Equal(t, "512", body.Value("Compression/subStream/bitRate")) // not sent: unchanged
	require.Equal(t, "7", body.Value("Compression/subStream/gop/cur"))   // one second at 7 fps
	require.False(t, videoChange(map[string]unifi.Video{"video1": {FPS: 18, Bitrate: 5120000}}, enc)(body))

	// below the lowest bitrate: the lowest; unknown choices: nothing changes
	body = parse(t, encXML)
	videoChange(map[string]unifi.Video{"video1": {Bitrate: 100000}}, enc)(body)
	require.Equal(t, "4096", body.Value("Compression/mainStream/bitRate"))
	body = parse(t, encXML)
	videoChange(map[string]unifi.Video{"video1": {FPS: 10, Bitrate: 100000}}, [2]encStream{})(body)
	require.Equal(t, "20", body.Value("Compression/mainStream/frame"))
	require.Equal(t, "6144", body.Value("Compression/mainStream/bitRate"))
}
