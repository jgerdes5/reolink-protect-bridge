package unifi

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// Samples captured from Protect.
func TestReadFeedback(t *testing.T) {
	in := []byte{
		0, 0, 0, 12, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0, 0xff, 0, 0, // bitrate: no limit
		0, 2, 0, 3, 0, 1, 1, // viewer v=1: keyframe
		0, 2, 0, 3, 0x80, 0, 1, // bit 15 set: no keyframe
		0, 1, 0, 0, // keyframe
		0, 5, 0, 2, 0, 3, // A/V drift
	}
	var got []Feedback
	err := ReadFeedback(bytes.NewReader(in), func(f Feedback) { got = append(got, f) })
	require.ErrorIs(t, err, io.EOF)
	require.Len(t, got, 5)

	want := []bool{false, true, false, true, false}
	types := []uint16{0, 2, 2, 1, 5}
	for i, f := range got {
		require.Equal(t, types[i], f.Type)
		require.Equal(t, want[i], f.WantsKeyframe(), i)
	}

	// a message cut off by the socket closing
	err = ReadFeedback(bytes.NewReader([]byte{0, 2, 0, 3, 0}), func(Feedback) { t.Fatal("partial message") })
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
