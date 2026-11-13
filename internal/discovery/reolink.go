package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
)

// Reolink's own UDP discovery (ports 2015/2018) is a P2P connect addressed to
// one device UID, not a "who is there" broadcast. So doorbells are found by
// scanning the local /24 for the Baichuan port and asking each host for a
// login nonce: the request carries no credentials, and only a Reolink device
// answers it with a Baichuan message.

// Doorbell is a Reolink device that answered on the Baichuan port.
type Doorbell struct {
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	Encryption string `json:"encryption,omitempty"` // offered by the device, e.g. "md5"
}

const (
	bcMagic       = 0x0ABCDEF0
	bcMsgLogin    = 1
	bcClassLegacy = 0x6514
	bcPort        = 9000
)

// nonceRequest is the header-only legacy login: the device replies with the
// nonce it would use for a login. Same bytes as pkg/baichuan Login sends first.
func nonceRequest() []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b[0:4], bcMagic)
	binary.LittleEndian.PutUint32(b[4:8], bcMsgLogin)
	// body length 0, channel 0, stream 0, message number 0
	binary.LittleEndian.PutUint16(b[16:18], 0xDC12)
	binary.LittleEndian.PutUint16(b[18:20], bcClassLegacy)
	return b
}

var errNotBaichuan = errors.New("not a Baichuan reply")

// ReadNonceReply reads one Baichuan message and checks it is a login reply.
// It returns the encryption type the device offers, if readable.
func ReadNonceReply(r io.Reader) (string, error) {
	hdr := make([]byte, 20)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return "", err
	}
	if binary.LittleEndian.Uint32(hdr[0:4]) != bcMagic {
		return "", errNotBaichuan
	}
	if binary.LittleEndian.Uint32(hdr[4:8]) != bcMsgLogin {
		return "", errNotBaichuan
	}
	bodyLen := binary.LittleEndian.Uint32(hdr[8:12])
	channel := hdr[12]
	class := binary.LittleEndian.Uint16(hdr[18:20])
	if bodyLen > 16384 {
		return "", errNotBaichuan
	}
	var offset uint32
	if class == 0x6414 || class == 0x0000 {
		ob := make([]byte, 4)
		if _, err := io.ReadFull(r, ob); err != nil {
			return "", err
		}
		offset = binary.LittleEndian.Uint32(ob)
		if offset > bodyLen {
			return "", errNotBaichuan
		}
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", nil // the header alone identifies a Reolink device
	}
	xml := baichuan.BCXOR(channel, body[offset:])
	return xmlValue(xml, "type"), nil
}

func xmlValue(doc []byte, tag string) string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	i := bytes.Index(doc, open)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(open):]
	j := bytes.Index(rest, end)
	if j < 0 || j > 64 {
		return ""
	}
	return cleanText(rest[:j])
}

// probeReolink asks one host for a login nonce.
func probeReolink(ctx context.Context, ip string, port int) (Doorbell, bool) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	dctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return Doorbell{}, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	if _, err = conn.Write(nonceRequest()); err != nil {
		return Doorbell{}, false
	}
	enc, err := ReadNonceReply(conn)
	if err != nil {
		return Doorbell{}, false
	}
	return Doorbell{IP: ip, Port: port, Encryption: enc}, true
}

// scanTargets are the hosts of the /24 around each local IPv4 address
// (private ranges only), without the host itself.
func scanTargets() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range localNets() {
		ip := n.IP.To4()
		if ip == nil || !ip.IsPrivate() {
			continue
		}
		for _, host := range hosts24(ip) {
			if !seen[host] {
				seen[host] = true
				out = append(out, host)
			}
		}
	}
	return out
}

// hosts24 lists x.y.z.1-254 for ip x.y.z.w, except ip itself.
func hosts24(ip net.IP) []string {
	ip = ip.To4()
	out := make([]string, 0, 253)
	for i := 1; i < 255; i++ {
		if byte(i) == ip[3] {
			continue
		}
		out = append(out, net.IPv4(ip[0], ip[1], ip[2], byte(i)).String())
	}
	return out
}

// Doorbells scans the local networks for Reolink devices on TCP 9000.
func Doorbells(ctx context.Context) ([]Doorbell, error) {
	targets := scanTargets()
	if len(targets) == 0 {
		return nil, errors.New("no private IPv4 network found on this host")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		found []Doorbell
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 64)
	)
	for _, ip := range targets {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if d, ok := probeReolink(ctx, ip, bcPort); ok {
				mu.Lock()
				found = append(found, d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	sort.Slice(found, func(i, j int) bool { return ipLess(found[i].IP, found[j].IP) })
	return found, nil
}
