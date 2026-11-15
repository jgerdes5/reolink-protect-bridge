package unifi

import (
	"encoding/binary"
	"io"
)

// Feedback is a message Protect's media server sends back on a stream
// socket: u16 type, u16 length (big-endian), body.
type Feedback struct {
	Type uint16
	Body []byte
}

const (
	FeedbackBitrate      = 0 // adaptive bitrate hint
	FeedbackKeyframe     = 1 // "send a keyframe now"
	FeedbackViewer       = 2 // u16: bit 15 clear and low bits set also asks for a keyframe
	FeedbackInvalidPoint = 3 // the server rejects the stream ("Invalid ingest point used")
	FeedbackRTT          = 4 // round-trip probe (a camera answers with onRtt)
	FeedbackEvent        = 5 // u16 complaint: 1 timestamp backward, 2 timestamp diff, 3 A/V drift
)

// WantsKeyframe reports whether a camera would force an IDR frame for this message.
func (f Feedback) WantsKeyframe() bool {
	switch f.Type {
	case FeedbackKeyframe:
		return true
	case FeedbackViewer:
		if len(f.Body) < 2 {
			return false
		}
		v := binary.BigEndian.Uint16(f.Body)
		return v&0x8000 == 0 && v&0x7fff != 0
	}
	return false
}

// ReadFeedback reads messages from r until it fails (Protect closing the
// socket). Reading also keeps the TCP window open.
func ReadFeedback(r io.Reader, fn func(Feedback)) error {
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		f := Feedback{Type: binary.BigEndian.Uint16(hdr[:])}
		if n := binary.BigEndian.Uint16(hdr[2:]); n > 0 {
			f.Body = make([]byte, n)
			if _, err := io.ReadFull(r, f.Body); err != nil {
				return err
			}
		}
		fn(f)
	}
}
