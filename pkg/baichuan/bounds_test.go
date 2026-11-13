package baichuan

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// Corrupt or hostile sizes in the bcmedia stream must fail with an error
// (the client then resets the parser), never panic or wait forever.
func TestMediaParserBounds(t *testing.T) {
	t.Parallel()

	video := func(payload, extra uint32) []byte {
		b := make([]byte, 24)
		binary.LittleEndian.PutUint32(b[0:4], bcmediaIFrameMin)
		copy(b[4:8], "H264")
		binary.LittleEndian.PutUint32(b[8:12], payload)
		binary.LittleEndian.PutUint32(b[12:16], extra)
		return b
	}
	adpcm := func(payload uint16) []byte {
		b := make([]byte, 16)
		binary.LittleEndian.PutUint32(b[0:4], bcmediaADPCM)
		binary.LittleEndian.PutUint16(b[4:6], payload)
		binary.LittleEndian.PutUint16(b[8:10], bcmediaADPCMHeader)
		return b
	}

	tests := []struct {
		name string
		in   []byte
	}{
		{"adpcm payload 0", adpcm(0)},
		{"adpcm payload 3", adpcm(3)},
		{"video payload huge", video(0xFFFFFFFF, 0)},
		{"video payload over limit", video(bcmediaMaxPayload+1, 0)},
		{"video extra huge", video(0, 0xFFFFFFFF)},
		{"video extra over limit", video(0, bcmediaMaxExtra+1)},
	}
	for _, tt := range tests {
		var p MediaParser
		if _, err := p.Append(tt.in); err == nil {
			t.Errorf("%s: Append() error = nil, want an error", tt.name)
		}
	}

	// sizes at the limit are still accepted (just incomplete here)
	var p MediaParser
	if _, err := p.Append(video(bcmediaMaxPayload, bcmediaMaxExtra)); err != nil {
		t.Errorf("at limit: Append() error = %v", err)
	}
}

func FuzzParseMediaPacket(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x30, 0x35, 0x77, 0x62, 0, 0, 0, 0}) // AAC, empty
	f.Add([]byte{0x30, 0x31, 0x77, 0x62, 2, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0x30, 0x30, 0x64, 0x63, 'H', '2', '6', '4', 1, 0, 0, 0, 255, 255, 255, 255, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		var p MediaParser
		_, _ = p.Append(data) // must not panic
		if p.buf.Len() > bcmediaMaxBuffered {
			t.Fatalf("buffer grew to %d", p.buf.Len())
		}
	})
}

type fakeTransport struct {
	r       *bytes.Reader
	written bytes.Buffer
}

func (f *fakeTransport) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *fakeTransport) Write(p []byte) (int, error) { return f.written.Write(p) }
func (f *fakeTransport) Close() error                { return nil }

func TestReadMessageBodyTooLarge(t *testing.T) {
	t.Parallel()

	hdr := make([]byte, 20)
	binary.LittleEndian.PutUint32(hdr[0:4], magicHeader)
	binary.LittleEndian.PutUint32(hdr[4:8], msgIDVideo)
	binary.LittleEndian.PutUint32(hdr[8:12], 0xFFFFFFFF)
	binary.LittleEndian.PutUint16(hdr[18:20], classLegacy)

	c := &Client{transport: &fakeTransport{r: bytes.NewReader(hdr)}}
	if _, err := c.readMessage(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readMessage() error = %v, want body length error", err)
	}
}

// Defense in depth: the commands that ring, play sounds, reboot or change
// chime settings never leave the client.
func TestWriteRequestRefusesForbiddenMsgIDs(t *testing.T) {
	t.Parallel()

	for _, id := range []uint32{263, 349, 350, 619, 483, 484, 485, 487, 610, 99, 23, 231, 428} {
		tr := &fakeTransport{}
		c := &Client{transport: tr, cfg: Config{Timeout: DefaultTimeout}}
		if err := c.writeRequest(request{MsgID: id, Class: classModernWithOffset}); err == nil {
			t.Errorf("msg id %d: writeRequest() error = nil", id)
		}
		if tr.written.Len() != 0 {
			t.Errorf("msg id %d: %d bytes written", id, tr.written.Len())
		}
	}

	tr := &fakeTransport{}
	c := &Client{transport: tr, cfg: Config{Timeout: DefaultTimeout}}
	if err := c.writeRequest(request{MsgID: msgIDPing, Class: classModernWithOffset}); err != nil {
		t.Fatalf("ping: writeRequest() error = %v", err)
	}
	if tr.written.Len() == 0 {
		t.Fatal("ping: nothing written")
	}
}

func TestSetNegotiatedEncryption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code       uint16
		mode       EncryptionMode
		negotiated bool
	}{
		{0xDD00, EncryptionNone, true},
		{0xDD01, EncryptionBC, true},
		{0xDD02, EncryptionAES, true},
		{0xDD03, EncryptionAES, true},
		{0xDD12, EncryptionAES, true},
		{0xDD7F, EncryptionNone, false}, // unknown: keep the current mode
	}
	for _, tt := range tests {
		c := &Client{}
		c.setNegotiatedEncryption(tt.code)
		if c.mode != tt.mode || c.negotiated != tt.negotiated {
			t.Errorf("%#x: mode %d negotiated %v, want %d %v", tt.code, c.mode, c.negotiated, tt.mode, tt.negotiated)
		}
	}
}

func TestAckWindowBounded(t *testing.T) {
	t.Parallel()

	store := map[uint32][]byte{
		12:                     {1},
		10 + udpRecvWindow*100: {1},
	}
	start, payload, ok := ackWindow(store, 10, true)
	if !ok || start != 10 {
		t.Fatalf("ackWindow() start=%d ok=%v", start, ok)
	}
	if len(payload) > udpRecvWindow {
		t.Fatalf("ackWindow() payload %d bytes, want <= %d", len(payload), udpRecvWindow)
	}
	if payload[1] != 1 {
		t.Fatalf("ackWindow() payload[1] = %d, want 1", payload[1])
	}
}

func TestHasVisitor(t *testing.T) {
	t.Parallel()

	if hasVisitor([]AlarmEvent{{Motion: true}}) {
		t.Error("motion only: hasVisitor = true")
	}
	if !hasVisitor([]AlarmEvent{{Motion: true}, {Visitor: true}}) {
		t.Error("visitor: hasVisitor = false")
	}
}
