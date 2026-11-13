package reolink

import (
	"github.com/pion/rtp"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
)

func TestRTPTimestampGuardMonotonic(t *testing.T) {
	guard := &rtpTimestampGuard{}

	// Initial timestamp
	ts1 := guard.next(1000)
	if ts1 != 1000 {
		t.Fatalf("expected 1000, got %d", ts1)
	}

	// Normal forward step
	ts2 := guard.next(4000)
	if ts2 != 4000 {
		t.Fatalf("expected 4000, got %d", ts2)
	}

	// Duplicate timestamp (delta == 0)
	ts3 := guard.next(4000)
	if ts3 != 4001 {
		t.Fatalf("expected 4001 for duplicate, got %d", ts3)
	}

	// Subsequent normal packet advances relative to its own delta
	ts4 := guard.next(7000)
	if ts4 != 7001 {
		t.Fatalf("expected 7001, got %d", ts4)
	}

	// Backward timestamp jump (e.g. camera clock reset to 0)
	ts5 := guard.next(0)
	if ts5 != 7002 {
		t.Fatalf("expected 7002 for backward jump, got %d", ts5)
	}

	// Resumed stream from new timeline
	ts6 := guard.next(3000)
	if ts6 != 10002 {
		t.Fatalf("expected 10002, got %d", ts6)
	}
}

func TestRTPTimestampGuardHugeForwardJump(t *testing.T) {
	guard := &rtpTimestampGuard{}

	guard.next(1000)
	// Jump forward by > 10 seconds (900000 ticks at 90kHz)
	ts := guard.next(2_000_000)
	if ts != 1001 {
		t.Fatalf("expected forward runaway jump to be guarded to 1001, got %d", ts)
	}

	// Subsequent packet advances from new timeline
	ts2 := guard.next(2_003_000)
	if ts2 != 4001 {
		t.Fatalf("expected 4001, got %d", ts2)
	}
}

func TestSetupBackchannelDisabled(t *testing.T) {
	c := &Client{
		backchannelEnabled: false,
	}
	err := c.SetupBackchannel()
	if err == nil || err.Error() != "reolink: talkback is disabled" {
		t.Fatalf("expected 'reolink: talkback is disabled', got %v", err)
	}
}

func TestSetupBackchannelClosed(t *testing.T) {
	c := &Client{
		backchannelEnabled: true,
		closed:             true,
	}
	err := c.SetupBackchannel()
	if err == nil || err.Error() != "reolink: client is closed" {
		t.Fatalf("expected 'reolink: client is closed', got %v", err)
	}
}

func TestTalkbackNonBlockingDrop(t *testing.T) {
	c := &Client{
		backchannelEnabled: true,
	}
	// Emulate talkMu being locked during background setup
	c.talkMu.Lock()

	media := &core.Media{Kind: core.KindAudio}
	codec := &core.Codec{Name: core.CodecPCMU}
	track := core.NewReceiver(media, codec)

	if err := c.AddTrack(media, codec, track); err != nil {
		t.Fatalf("AddTrack failed: %v", err)
	}

	// Calling Handler while talkMu is locked should return immediately (dropping the packet)
	// and NOT block on the lock.
	done := make(chan struct{})
	go func() {
		c.sender.Handler(&core.Packet{Payload: []byte{0, 1, 2, 3}})
		close(done)
	}()

	select {
	case <-done:
		// success: handler returned immediately without blocking
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("c.sender.Handler blocked while talkMu was locked!")
	}

	c.talkMu.Unlock()
}

func TestStartReturnsNilWhenClosed(t *testing.T) {
	ch := make(chan baichuan.MediaPacket)
	c := &Client{
		receivers: []*core.Receiver{
			core.NewReceiver(&core.Media{Kind: core.KindVideo}, &core.Codec{Name: core.CodecH264}),
		},
		reader: &baichuan.MediaReader{
			Packets: ch,
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- c.Start()
	}()

	_ = c.Close()
	close(ch)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error on intentional close, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Start() did not return after Close()")
	}
}

// A second talk session on a running client gets its audio to the sender:
// closing the first session's track only detaches the sender, and a sender
// that was closed for real is replaced.
func TestTalkbackSecondSession(t *testing.T) {
	c := &Client{backchannelEnabled: true}
	media := &core.Media{Kind: core.KindAudio}
	codec := &core.Codec{Name: core.CodecPCMU, ClockRate: 8000}
	pkt := &rtp.Packet{Payload: []byte{1, 2, 3}}
	c.closed = true // the lazy talk setup the handler starts returns at once

	first := core.NewReceiver(media, codec)
	if err := c.AddTrack(media, codec, first); err != nil {
		t.Fatal(err)
	}
	first.Close() // the talk session ended

	second := core.NewReceiver(media, codec)
	if err := c.AddTrack(media, codec, second); err != nil {
		t.Fatal(err)
	}
	second.WriteRTP(pkt)
	if c.sender.Packets != 1 {
		t.Fatalf("second session: sender got %d packets, want 1", c.sender.Packets)
	}

	c.sender.Close() // closed for real
	third := core.NewReceiver(media, codec)
	if err := c.AddTrack(media, codec, third); err != nil {
		t.Fatal(err)
	}
	third.WriteRTP(pkt)
	if c.sender.State() == "closed" || c.sender.Packets != 1 {
		t.Fatalf("third session: state %q, packets %d", c.sender.State(), c.sender.Packets)
	}
}

func TestIsUID(t *testing.T) {
	tests := map[string]bool{
		"95270000ABCDEFGH":           true,
		"95270000ABCDEFGHIJ":         true,
		"192.168.1.20":               false,
		"localhost":                  false,
		"reolink-doorbell":           false, // hostname, not a UID
		"reolinkdoorbell1":           false, // lowercase
		"fe80::1234:5678:9abc:def0":  false, // IPv6 literal
		"2001:DB8:0:0:0:0:0:1":       false,
		"95270000ABCDEFG":            false, // too short
		"doorbell.home.arpa":         false,
		"FE80000000000000ABCDEF0123": true, // UID-shaped, not an IP
	}
	for host, want := range tests {
		if got := isUID(host); got != want {
			t.Errorf("isUID(%q) = %v, want %v", host, got, want)
		}
	}
}

// A connection that stays up while the stream freezes must end Start, so the
// source reconnects.
func TestStartMediaWatchdog(t *testing.T) {
	old := mediaTimeout
	mediaTimeout = 50 * time.Millisecond
	defer func() { mediaTimeout = old }()

	c := &Client{
		receivers: []*core.Receiver{
			core.NewReceiver(&core.Media{Kind: core.KindVideo}, &core.Codec{Name: core.CodecH264}),
		},
		reader: &baichuan.MediaReader{
			Packets: make(chan baichuan.MediaPacket), // never sends, never closes
		},
	}

	done := make(chan error, 1)
	go func() { done <- c.Start() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start() = nil, want a no-media error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return on a frozen stream")
	}
}
