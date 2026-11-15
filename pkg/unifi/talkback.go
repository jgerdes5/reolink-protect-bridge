package unifi

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Talkback as Protect sets it up for a camera: ChangeTalkbackSettings with
// typeFmt "aac", typeIn "serverudp", bindPort 7004, 22050 Hz mono. The camera
// listens on that UDP port and Protect sends one ADTS AAC frame per datagram
// to the camera's IP (the ip in hello). While HomeKit talks, Protect switches
// to typeFmt "opus", typeIn "serverudp-rtp" (24 kHz): Opus in RTP on the same
// port.

const talkbackPort = 7004

type talkback struct {
	mu   sync.Mutex
	conn *net.UDPConn
	port int
	// read per datagram by the listener
	opus    atomic.Bool                     // the format Protect set up last: Opus/RTP, else AAC
	allowed atomic.Pointer[map[string]bool] // the console's addresses
}

// talkbackSettings is the reply Protect validates (TalkbackSettingsSchema): all
// fields present, filterAddr/filterPort may be null.
type talkbackSettings struct {
	TypeFmt       string  `json:"typeFmt"`
	TypeIn        string  `json:"typeIn"`
	BindAddr      string  `json:"bindAddr"`
	BindPort      int     `json:"bindPort"`
	FilterAddr    *string `json:"filterAddr"`
	FilterPort    *int    `json:"filterPort"`
	Channels      int     `json:"channels"`
	SamplingRate  int     `json:"samplingRate"`
	BitsPerSample int     `json:"bitsPerSample"`
	Quality       int     `json:"quality"`
	URL           string  `json:"url"`
}

func (c *Camera) changeTalkbackSettings(payload json.RawMessage) any {
	s := talkbackSettings{
		TypeFmt: "aac", TypeIn: "serverudp", BindAddr: "0.0.0.0", BindPort: talkbackPort,
		Channels: 1, SamplingRate: 22050, BitsPerSample: 16, Quality: 100,
	}
	_ = json.Unmarshal(payload, &s)
	if s.BindPort <= 0 {
		s.BindPort = talkbackPort
	}

	if s.TypeFmt != "aac" && (s.TypeFmt != "opus" || c.TalkOpus == nil) {
		c.Log.Warn().Msgf("[unifi] %s: talkback format %q not supported", c.Name, s.TypeFmt)
		return s
	}
	c.talk.opus.Store(s.TypeFmt == "opus") // only this format is accepted
	if err := c.listenTalkback(s.BindPort); err != nil {
		c.Log.Warn().Err(err).Msgf("[unifi] %s: talkback listener", c.Name)
	}
	return s
}

// listenTalkback keeps one UDP listener for the camera's lifetime.
func (c *Camera) listenTalkback(port int) error {
	if port <= 0 {
		port = talkbackPort
	}

	// Only the Protect console may play audio on the speaker. Resolved before
	// the lock; without its address nothing listens (never open to anyone).
	host := c.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h // "console:443" or "[fd00::1]:443"
	}
	host = strings.Trim(host, "[]")
	allowed := map[string]bool{}
	if ip := net.ParseIP(host); ip != nil {
		allowed[ip.String()] = true
	} else if ips, err := net.LookupIP(host); err == nil {
		for _, ip := range ips {
			allowed[ip.String()] = true
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("can't resolve the console address %q", c.Host)
	}
	c.talk.allowed.Store(&allowed) // resolved again on every settings message

	c.talk.mu.Lock()
	defer c.talk.mu.Unlock()

	if c.talk.conn != nil {
		if c.talk.port == port {
			return nil
		}
		_ = c.talk.conn.Close()
		c.talk.conn = nil
	}

	addr, err := net.ResolveUDPAddr("udp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	c.talk.conn, c.talk.port = conn, port
	c.Log.Info().Msgf("[unifi] %s: talkback listening on udp/%d", c.Name, port)

	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // closed
			}
			if a := c.talk.allowed.Load(); a == nil || !(*a)[from.IP.String()] {
				continue
			}
			switch {
			case c.talk.opus.Load():
				if pkt := rtpPayload(buf[:n]); len(pkt) > 0 && c.TalkOpus != nil {
					c.TalkOpus(append([]byte(nil), pkt...))
				}
			case n > 7 && buf[0] == 0xFF && buf[1]&0xF0 == 0xF0: // ADTS sync word
				c.Talk(append([]byte(nil), buf[:n]...))
			}
		}
	}()
	return nil
}

// rtpPayload is an RTP packet's payload (RFC 3550), nil if b isn't one.
func rtpPayload(b []byte) []byte {
	if len(b) < 12 || b[0]>>6 != 2 {
		return nil
	}
	off := 12 + 4*int(b[0]&0x0F)
	if b[0]&0x10 != 0 { // header extension
		if len(b) < off+4 {
			return nil
		}
		off += 4 + 4*int(binary.BigEndian.Uint16(b[off+2:]))
	}
	end := len(b)
	if b[0]&0x20 != 0 && end > 0 { // padding
		end -= int(b[end-1])
	}
	if off >= end {
		return nil
	}
	return b[off:end]
}

// closeTalkback stops the listener (when the camera stops).
func (c *Camera) closeTalkback() {
	c.talk.mu.Lock()
	defer c.talk.mu.Unlock()
	if c.talk.conn != nil {
		_ = c.talk.conn.Close()
		c.talk.conn = nil
	}
}
