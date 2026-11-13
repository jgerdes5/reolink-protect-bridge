package unifi

import (
	"encoding/base64"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/bits"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/stretchr/testify/require"
)

// The Reolink doorbell's main-stream SPS (High, 2560x1920), which has no VUI.
const doorbellSPS = "Z2QAM6wVFKAoAPGQ"

func TestWithTiming(t *testing.T) {
	// testSPS already carries VUI timing and is left as it is
	sps, _ := base64.StdEncoding.DecodeString(testSPS)
	require.Equal(t, sps, withTiming(sps, 20))

	for _, src := range []string{doorbellSPS} {
		sps, _ := base64.StdEncoding.DecodeString(src)
		before := h264.DecodeSPS(sps)
		require.NotNil(t, before)

		out := withTiming(sps, 20)
		require.NotEqual(t, sps, out)

		after := h264.DecodeSPS(out)
		require.NotNil(t, after)
		require.Equal(t, before.Width(), after.Width())
		require.Equal(t, before.Height(), after.Height())
		units, scale, ok := readTiming(out)
		require.True(t, ok)
		require.Equal(t, uint32(1), units)
		require.Equal(t, uint32(40), scale) // 20 fps

		// already has a VUI: unchanged
		require.Equal(t, out, withTiming(out, 15))
	}

	// no fps: unchanged
	sps, _ = base64.StdEncoding.DecodeString(doorbellSPS)
	require.Equal(t, sps, withTiming(sps, 0))
}

// readTiming reads num_units_in_tick and time_scale from an SPS whose VUI
// starts with timing info (as withTiming writes it).
func readTiming(sps []byte) (units, scale uint32, ok bool) {
	rbsp := unescapeRBSP(sps[1:])
	pos, ok := vuiFlagPos(rbsp)
	if !ok {
		return 0, 0, false
	}
	r := bits.NewReader(rbsp)
	for i := 0; i < pos; i++ {
		r.ReadBit()
	}
	if r.ReadBit() != 1 || r.ReadBits(4) != 0 || r.ReadBit() != 1 {
		return 0, 0, false
	}
	return r.ReadBits(32), r.ReadBits(32), !r.EOF
}

func TestRBSPEscape(t *testing.T) {
	raw := []byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00}
	esc := escapeRBSP(raw)
	require.Equal(t, []byte{0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x00, 0x05, 0x00, 0x00}, esc)
	require.Equal(t, raw, unescapeRBSP(esc))
}

// In-band SPS in keyframes is rewritten like the one in the decoder config;
// the shared frame is not modified.
func TestConsumerReplacesInbandSPS(t *testing.T) {
	sps, _ := base64.StdEncoding.DecodeString(doorbellSPS)
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	idr := []byte{0x65, 0x88, 0x84}

	c := NewConsumer(StreamOptions{FPS: 20})
	c.video = &core.Codec{Name: core.CodecH264, ClockRate: 90000}

	frame := avcc(sps, pps, idr)
	orig := append([]byte(nil), frame...)
	out := c.replaceSPS(frame)

	require.Equal(t, orig, frame)
	gotSPS, gotPPS := parameterSets(out)
	require.Equal(t, withTiming(sps, 20), gotSPS)
	require.Equal(t, pps, gotPPS)
	require.Equal(t, avcc(withTiming(sps, 20), pps, idr), out)

	// frames without SPS are passed through as they are
	p := avcc([]byte{0x41, 0x9a})
	require.Equal(t, &p[0], &c.replaceSPS(p)[0])
}
