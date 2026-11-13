package baichuan

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pierrec/lz4/v4"
	"github.com/stretchr/testify/require"
)

func tlv(t byte, value ...[]byte) []byte {
	v := bytes.Join(value, nil)
	b := []byte{t, 0, 0}
	binary.LittleEndian.PutUint16(b[1:], uint16(len(v)))
	return append(b, v...)
}

func u16s(vals ...int) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func lz4Frame(t *testing.T, data []byte) []byte {
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	_, err := w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestDecodeAIBoxes(t *testing.T) {
	box := tlv(4, u16s(100, 50, 300, 450, 87))                         // x1,y1,x2,y2,conf
	tracked := tlv(4, u16s(500, 60, 600, 200, 70), u16s(42))           // + track id (len 12 -> not a box length)
	tracked13 := tlv(4, u16s(500, 60, 600, 200, 70), []byte{42, 0, 0}) // len 13
	inner := tlv(1, tlv(1, tracked13))                                 // walked with fresh wrapper types

	body := bytes.Join([][]byte{
		tlv(1, tlv(1, make([]byte, 8))),    // the marker's "01 0B 00 01 08 ..." preamble record
		tlv(3, u16s(1920, 1080)),           // frame size 03 04 00 w h
		tlv(2, box),                        // a box under wrapper 255/2
		tlv(2, tlv(2, lz4Frame(t, inner))), // LZ4 frame under 255/2
		tlv(5, box),                        // the same rectangle again (another view): merged
	}, nil)
	header := tlv(0xff, body)
	require.True(t, aiMarker(header, 0), "% x", header[:8])
	_ = tracked

	boxes, w, h := DecodeAIBoxes(header)
	require.Equal(t, 1920, w)
	require.Equal(t, 1080, h)
	require.Len(t, boxes, 2)
	require.Equal(t, AIBox{X1: 100, Y1: 50, X2: 300, Y2: 450, Confidence: 87, Type1: 255, Type2: 2, TrackID: -1}, boxes[0])
	require.Equal(t, 500, boxes[1].X1)
	require.Equal(t, 42, boxes[1].TrackID)
	require.Equal(t, 1, boxes[1].Type1)

	// I-frames carry an 8-byte prefix before the marker
	boxes, _, _ = DecodeAIBoxes(append(make([]byte, 8), header...))
	require.Len(t, boxes, 2)

	// no marker (sub stream counter, plain timestamp): nothing
	boxes, w, h = DecodeAIBoxes([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	require.Empty(t, boxes)
	require.Equal(t, 896, w)
	require.Equal(t, 480, h)

	// truncated / garbage input must not panic
	for i := range header {
		DecodeAIBoxes(header[:i])
	}
}
