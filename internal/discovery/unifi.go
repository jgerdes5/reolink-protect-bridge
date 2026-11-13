// Package discovery finds the doorbell and the UniFi console on the local
// network. It only runs when the user asks for it and never sends credentials.
package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Console is a Ubiquiti device that answered the discovery probe.
type Console struct {
	IP       string `json:"ip"`
	MAC      string `json:"mac,omitempty"`
	Name     string `json:"name,omitempty"`
	Platform string `json:"platform,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	SystemID string `json:"systemId,omitempty"` // e.g. "ea15"
	Console  bool   `json:"console"`            // looks like a UniFi OS console (may run Protect)
}

// Ubiquiti discovery TLV types (see pyunifiwire SPEC 1a).
const (
	tlvHWAddr   = 0x01
	tlvIPInfo   = 0x02
	tlvFirmware = 0x03
	tlvHostname = 0x0b
	tlvPlatform = 0x0c
	tlvSystemID = 0x10
)

var ubntProbe = []byte{0x01, 0x00, 0x00, 0x00}

// ParseUbiquiti decodes a discovery reply: version, command, 2-byte big endian
// length, then TLVs (1-byte type, 2-byte big endian length, value). Unknown
// types are skipped. from is the sender, preferred among several addresses.
func ParseUbiquiti(buf []byte, from net.IP) (Console, error) {
	var c Console
	if len(buf) < 4 {
		return c, errors.New("short packet")
	}
	if buf[0] != 1 && buf[0] != 2 {
		return c, fmt.Errorf("unknown version %d", buf[0])
	}
	n := int(binary.BigEndian.Uint16(buf[2:4]))
	if n == 0 {
		return c, errors.New("no payload (a probe, not a reply)")
	}
	if 4+n > len(buf) {
		return c, errors.New("truncated packet")
	}
	body := buf[4 : 4+n]

	var ips []net.IP
	for len(body) > 0 {
		if len(body) < 3 {
			return c, errors.New("truncated TLV header")
		}
		typ, l := body[0], int(binary.BigEndian.Uint16(body[1:3]))
		if 3+l > len(body) {
			return c, fmt.Errorf("truncated TLV %#02x", typ)
		}
		v := body[3 : 3+l]
		body = body[3+l:]

		switch typ {
		case tlvHWAddr:
			if l == 6 {
				c.MAC = net.HardwareAddr(v).String()
			}
		case tlvIPInfo:
			if l == 10 {
				if c.MAC == "" {
					c.MAC = net.HardwareAddr(v[:6]).String()
				}
				ips = append(ips, net.IP(append([]byte(nil), v[6:10]...)))
			}
		case tlvFirmware:
			c.Firmware = cleanText(v)
		case tlvHostname:
			c.Name = cleanText(v)
		case tlvPlatform:
			c.Platform = cleanText(v)
		case tlvSystemID:
			if l == 2 {
				c.SystemID = fmt.Sprintf("%04x", binary.LittleEndian.Uint16(v))
			}
		}
	}

	for _, ip := range ips {
		if from != nil && ip.Equal(from) {
			c.IP = ip.String()
		}
	}
	if c.IP == "" {
		for _, ip := range ips {
			if !ip.IsUnspecified() {
				c.IP = ip.String()
				break
			}
		}
	}
	if c.IP == "" && from != nil {
		c.IP = from.String()
	}
	c.Console = isConsole(c.Platform, c.Firmware)
	return c, nil
}

func cleanText(v []byte) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(v)))
}

// consolePrefixes are platforms of UniFi OS consoles and NVRs.
var consolePrefixes = []string{"UDM", "UDR", "UDW", "UCK", "UNVR", "UCG", "UXG", "UDMA", "EFG", "ENVR", "UNAS", "UX"}

func isConsole(platform, firmware string) bool {
	p := strings.ToUpper(strings.ReplaceAll(platform, "-", ""))
	for _, prefix := range consolePrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	// UniFi OS firmware strings carry the platform first, e.g. "UDMPRO.al324.v4..."
	f := strings.ToUpper(firmware)
	for _, prefix := range consolePrefixes {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

// Consoles broadcasts the Ubiquiti discovery probe and collects replies for
// the given duration (3 s if 0). Consoles come first.
func Consoles(ctx context.Context, wait time.Duration) ([]Console, error) {
	if wait <= 0 {
		wait = 3 * time.Second
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()

	targets := []*net.UDPAddr{
		{IP: net.IPv4bcast, Port: 10001},
		{IP: net.IPv4(233, 89, 188, 1), Port: 10001},
	}
	for _, ip := range ipv4Broadcasts() {
		targets = append(targets, &net.UDPAddr{IP: ip, Port: 10001})
	}
	send := func() {
		for _, t := range targets {
			_, _ = conn.WriteToUDP(ubntProbe, t)
		}
	}
	send()

	deadline := time.Now().Add(wait)
	resend := time.Now().Add(time.Second)
	found := map[string]Console{}
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		next := resend
		if deadline.Before(next) {
			next = deadline
		}
		_ = conn.SetReadDeadline(next)
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if time.Now().After(resend) {
					send() // UDP may be lost; devices answer every probe
					resend = time.Now().Add(time.Second)
				}
				continue
			}
			return nil, err
		}
		c, err := ParseUbiquiti(buf[:n], addr.IP)
		if err != nil {
			continue
		}
		key := c.MAC
		if key == "" {
			key = c.IP
		}
		found[key] = c
	}

	list := make([]Console, 0, len(found))
	for _, c := range found {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Console != list[j].Console {
			return list[i].Console
		}
		return ipLess(list[i].IP, list[j].IP)
	})
	return list, ctx.Err()
}

func ipLess(a, b string) bool {
	x, y := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if x == nil || y == nil {
		return a < b
	}
	return binary.BigEndian.Uint32(x) < binary.BigEndian.Uint32(y)
}

// localNets are the host's IPv4 networks (up, not loopback).
func localNets() []*net.IPNet {
	var nets []*net.IPNet
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			nets = append(nets, &net.IPNet{IP: ipn.IP.To4(), Mask: ipn.Mask})
		}
	}
	return nets
}

func ipv4Broadcasts() []net.IP {
	var out []net.IP
	for _, n := range localNets() {
		mask := n.Mask
		if len(mask) == 16 {
			mask = mask[12:]
		}
		if len(mask) != 4 {
			continue
		}
		b := make(net.IP, 4)
		for i := range 4 {
			b[i] = n.IP[i] | ^mask[i]
		}
		out = append(out, b)
	}
	return out
}
