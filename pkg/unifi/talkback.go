package unifi

import (
	"encoding/json"
	"net"
	"strconv"
	"sync"
)

// Talkback as Protect 7.3 sets it up for a camera (service.js
// updateTalkbackSettings): ChangeTalkbackSettings with typeFmt "aac", typeIn
// "serverudp", bindPort 7004, 22050 Hz mono. The camera listens on that UDP
// port and Protect sends one ADTS AAC frame per datagram to the camera's IP
// (the ip in hello). A real camera's ubnt_talkback does exactly this
// (InputServerUDP -> InputChainParserADTS -> DecoderAAC). Opus over RTP is used
// only for HomeKit and is not offered here.

const talkbackPort = 7004

type talkback struct {
	mu   sync.Mutex
	conn *net.UDPConn
	port int
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

	if s.TypeFmt != "aac" {
		c.Log.Warn().Msgf("[unifi] %s: talkback format %q not supported, only aac", c.Name, s.TypeFmt)
	} else if err := c.listenTalkback(s.BindPort); err != nil {
		c.Log.Warn().Err(err).Msgf("[unifi] %s: talkback listener", c.Name)
	}
	return s
}

// listenTalkback keeps one UDP listener for the camera's lifetime.
func (c *Camera) listenTalkback(port int) error {
	if port <= 0 {
		port = talkbackPort
	}

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

	// Only the Protect console may play audio on the speaker.
	allowed := map[string]bool{}
	if ips, err := net.LookupIP(c.Host); err == nil {
		for _, ip := range ips {
			allowed[ip.String()] = true
		}
	}

	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // closed
			}
			if len(allowed) > 0 && !allowed[from.IP.String()] {
				continue
			}
			if n > 7 && buf[0] == 0xFF && buf[1]&0xF0 == 0xF0 { // ADTS sync word
				c.Talk(append([]byte(nil), buf[:n]...))
			}
		}
	}()
	return nil
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
