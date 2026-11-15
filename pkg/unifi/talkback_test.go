package unifi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRTPPayload(t *testing.T) {
	hdr := []byte{0x80, 0x6f, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1}
	require.Equal(t, []byte{0xfc, 0xaa}, rtpPayload(append(hdr, 0xfc, 0xaa)))

	// one CSRC and a one-word header extension
	ext := []byte{0x91, 0x6f, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 2, 0xbe, 0xde, 0, 1, 1, 2, 3, 4, 0xfc}
	require.Equal(t, []byte{0xfc}, rtpPayload(ext))

	// padding: last byte is the count
	pad := append([]byte{0xa0}, hdr[1:]...)
	require.Equal(t, []byte{0xfc}, rtpPayload(append(pad, 0xfc, 0, 2)))

	require.Nil(t, rtpPayload([]byte{0xff, 0xf1, 0x50})) // ADTS, not RTP
	require.Nil(t, rtpPayload(append(append([]byte(nil), pad...), 9)))
}
