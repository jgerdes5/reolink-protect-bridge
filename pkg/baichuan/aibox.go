package baichuan

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/pierrec/lz4/v4"
)

// AIBox is an object rectangle from the camera's own AI, in the coordinate
// space FrameWidth x FrameHeight of DecodeAIBoxes.
//
// Reolink cameras put their detections into the additional header of video
// frames on the main stream ("AI Mark"): nested TLVs (type u8, length u16 LE),
// partly LZ4-frame compressed, leaves x1,y1,x2,y2,confidence as u16 LE. The
// same box appears once per wrapper ("view"/class), so copies with equal
// rectangles are merged. Format as decoded by woutersteegers/reolink-unifi-
// protect (deploy/ai-sidecar/decode.mjs, from nodelink-js); the D340W
// firmware writes these TLVs in media_bcframe.cpp with LZ4F ("compress ai
// data").
type AIBox struct {
	X1, Y1, X2, Y2 int
	Confidence     int
	Type1, Type2   int // wrapper types; which one is the class differs by firmware
	TrackID        int // from the extra bytes when present, else -1
}

const (
	defaultAIWidth  = 896
	defaultAIHeight = 480
	maxAIDecoded    = 256 << 10
)

var lz4Magic = []byte{0x04, 0x22, 0x4d, 0x18}

func aiMarker(b []byte, off int) bool {
	return len(b) >= off+8 && b[off] == 0xff && b[off+2] == 0x00 && b[off+3] == 0x01 &&
		b[off+4] == 0x0b && b[off+5] == 0x00 && b[off+6] == 0x01 && b[off+7] == 0x08
}

// DecodeAIBoxes returns the distinct boxes in a frame's additional header
// and their coordinate space (896x480 unless the header says otherwise).
// Headers without the AI marker yield no boxes.
func DecodeAIBoxes(extra []byte) (boxes []AIBox, width, height int) {
	width, height = defaultAIWidth, defaultAIHeight
	off := -1
	switch {
	case aiMarker(extra, 8): // I-frames: 8-byte prefix (unix time ...)
		off = 8
	case aiMarker(extra, 0):
		off = 0
	}
	if off < 0 {
		return nil, width, height
	}

	var all []AIBox
	walkAI(extra, off, len(extra), 0, 0, &all, 0)

	// frame size TLV 03 04 00 w h
	for i := off + 8; i+7 <= len(extra); i++ {
		if extra[i] == 0x03 && extra[i+1] == 0x04 && extra[i+2] == 0x00 {
			w := int(binary.LittleEndian.Uint16(extra[i+3:]))
			h := int(binary.LittleEndian.Uint16(extra[i+5:]))
			if w >= 64 && w <= 8192 && h >= 64 && h <= 8192 {
				width, height = w, h
				break
			}
		}
	}

	// merge copies of the same rectangle, keeping the best confidence and a track id
	for _, b := range all {
		merged := false
		for i := range boxes {
			o := &boxes[i]
			if o.X1 == b.X1 && o.Y1 == b.Y1 && o.X2 == b.X2 && o.Y2 == b.Y2 {
				o.Confidence = max(o.Confidence, b.Confidence)
				if o.TrackID < 0 {
					o.TrackID = b.TrackID
				}
				merged = true
				break
			}
		}
		if !merged {
			boxes = append(boxes, b)
		}
	}
	return boxes, width, height
}

func walkAI(b []byte, pos, end, type1, type2 int, out *[]AIBox, depth int) {
	if depth > 16 {
		return
	}
	for pos+3 <= end {
		t := int(b[pos])
		length := int(binary.LittleEndian.Uint16(b[pos+1:]))
		recEnd := min(pos+3+length, end)

		isBox := (t == 4 && (length == 10 || length == 13 || length == 14)) || (t == 2 && length == 10)
		if isBox && type1 != 0 && type2 != 0 {
			if pos+13 <= end {
				box := AIBox{
					X1:         int(binary.LittleEndian.Uint16(b[pos+3:])),
					Y1:         int(binary.LittleEndian.Uint16(b[pos+5:])),
					X2:         int(binary.LittleEndian.Uint16(b[pos+7:])),
					Y2:         int(binary.LittleEndian.Uint16(b[pos+9:])),
					Confidence: int(binary.LittleEndian.Uint16(b[pos+11:])),
					Type1:      type1, Type2: type2, TrackID: -1,
				}
				if recEnd-(pos+13) >= 2 {
					box.TrackID = int(binary.LittleEndian.Uint16(b[pos+13:]))
				}
				if box.X2 > box.X1 && box.Y2 > box.Y1 {
					*out = append(*out, box)
				}
			}
			pos = recEnd
			continue
		}

		if type1 == 255 && type2 == 2 && t == 2 && length >= 4 && bytes.HasPrefix(b[pos+3:recEnd], lz4Magic) {
			if dec := unlz4(b[pos+3 : recEnd]); dec != nil {
				walkAI(dec, 0, len(dec), 0, 0, out, depth+1)
			}
			pos = recEnd
			continue
		}

		if length > 0 {
			n1, n2 := type1, type2
			if type1 == 0 {
				n1 = t
			} else if type2 == 0 {
				n2 = t
			}
			walkAI(b, pos+3, recEnd, n1, n2, out, depth+1)
		}
		pos = recEnd
	}
}

func unlz4(frame []byte) []byte {
	r := lz4.NewReader(bytes.NewReader(frame))
	dec, err := io.ReadAll(io.LimitReader(r, maxAIDecoded))
	if err != nil {
		return nil
	}
	return dec
}
