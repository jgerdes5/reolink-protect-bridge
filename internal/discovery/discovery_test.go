package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/stretchr/testify/require"
)

func tlv(typ byte, v []byte) []byte {
	return append([]byte{typ, byte(len(v) >> 8), byte(len(v))}, v...)
}

func ubntReply(tlvs ...[]byte) []byte {
	body := bytes.Join(tlvs, nil)
	return append([]byte{1, 0, byte(len(body) >> 8), byte(len(body))}, body...)
}

func TestParseUbiquiti(t *testing.T) {
	mac := []byte{0x74, 0xac, 0xb9, 0x01, 0x02, 0x03}
	pkt := ubntReply(
		tlv(tlvIPInfo, append(append([]byte{}, mac...), 192, 168, 1, 1)),
		tlv(tlvHWAddr, mac),
		tlv(0x0a, []byte{0, 0, 1, 0}), // uptime, skipped
		tlv(tlvHostname, []byte("Dream Machine")),
		tlv(tlvPlatform, []byte("UDMPRO")),
		tlv(0x17, []byte{0, 0, 0, 0}),
		tlv(tlvFirmware, []byte("UDMPRO.al324.v4.1.13.abc.250101.0101")),
		tlv(tlvSystemID, []byte{0x15, 0xea}),
		tlv(0x2b, make([]byte, 16)),
	)
	c, err := ParseUbiquiti(pkt, net.IPv4(192, 168, 1, 1))
	require.NoError(t, err)
	require.Equal(t, Console{
		IP: "192.168.1.1", MAC: "74:ac:b9:01:02:03", Name: "Dream Machine",
		Platform: "UDMPRO", Firmware: "UDMPRO.al324.v4.1.13.abc.250101.0101",
		SystemID: "ea15", Console: true,
	}, c)
}

func TestParseUbiquitiCamera(t *testing.T) {
	// a real G5 PTZ answer shape (SPEC 1a): not a console
	mac := []byte{0x1c, 0x6a, 0x1b, 0xaa, 0xbb, 0xcc}
	pkt := ubntReply(
		tlv(tlvIPInfo, append(append([]byte{}, mac...), 10, 0, 0, 7)),
		tlv(tlvHostname, []byte("G5 PTZ")),
		tlv(tlvPlatform, []byte("UVC G5 PTZ")),
		tlv(tlvFirmware, []byte("UVC.SAV530q.v5.3.95.67.148b9a3.260612.0645")),
		tlv(tlvSystemID, []byte{0x9b, 0xa5}),
	)
	c, err := ParseUbiquiti(pkt, net.IPv4(10, 0, 0, 7))
	require.NoError(t, err)
	require.Equal(t, "a59b", c.SystemID)
	require.Equal(t, "10.0.0.7", c.IP)
	require.Equal(t, "1c:6a:1b:aa:bb:cc", c.MAC)
	require.False(t, c.Console)
}

func TestParseUbiquitiBad(t *testing.T) {
	for _, pkt := range [][]byte{
		nil,
		{1, 0, 0, 0},                  // our own probe
		{9, 0, 0, 1, 0},               // unknown version
		{1, 0, 0, 10, 1, 0},           // truncated
		{1, 0, 0, 4, 0x0b, 0, 9, 'x'}, // TLV longer than payload
	} {
		_, err := ParseUbiquiti(pkt, nil)
		require.Error(t, err, "%x", pkt)
	}
}

func TestNonceRequest(t *testing.T) {
	require.Equal(t, []byte{
		0xf0, 0xde, 0xbc, 0x0a, 1, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0x12, 0xdc, 0x14, 0x65,
	}, nonceRequest())
}

func nonceReply(class uint16, withOffset bool) []byte {
	xml := []byte(`<?xml version="1.0" encoding="UTF-8" ?><body><Encryption version="1.1"><type>md5</type><nonce>abc123</nonce></Encryption></body>`)
	body := baichuan.BCXOR(0, xml)
	hdr := make([]byte, 20)
	binary.LittleEndian.PutUint32(hdr[0:4], bcMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], bcMsgLogin)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(body)))
	binary.LittleEndian.PutUint16(hdr[16:18], 0xDD01)
	binary.LittleEndian.PutUint16(hdr[18:20], class)
	if withOffset {
		hdr = append(hdr, 0, 0, 0, 0)
	}
	return append(hdr, body...)
}

func TestReadNonceReply(t *testing.T) {
	enc, err := ReadNonceReply(bytes.NewReader(nonceReply(0x6614, false)))
	require.NoError(t, err)
	require.Equal(t, "md5", enc)

	enc, err = ReadNonceReply(bytes.NewReader(nonceReply(0x0000, true)))
	require.NoError(t, err)
	require.Equal(t, "md5", enc)

	_, err = ReadNonceReply(bytes.NewReader([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")))
	require.Error(t, err)
	_, err = ReadNonceReply(bytes.NewReader([]byte{1, 2}))
	require.Error(t, err)
}

// TestProbeReolink runs against a fake device on loopback.
func TestProbeReolink(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req := make([]byte, 20)
		_, _ = io.ReadFull(conn, req)
		got <- req
		_, _ = conn.Write(nonceReply(0x6614, false))
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	d, ok := probeReolink(context.Background(), "127.0.0.1", port)
	require.True(t, ok)
	require.Equal(t, Doorbell{IP: "127.0.0.1", Port: port, Encryption: "md5"}, d)
	require.Equal(t, nonceRequest(), <-got) // no credentials, just the header
}

func TestHosts24(t *testing.T) {
	h := hosts24(net.IPv4(192, 168, 5, 10))
	require.Len(t, h, 253)
	require.Equal(t, "192.168.5.1", h[0])
	require.NotContains(t, h, "192.168.5.10")
	require.Equal(t, "192.168.5.254", h[len(h)-1])
}
