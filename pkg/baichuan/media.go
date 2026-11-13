package baichuan

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

const (
	bcmediaInfoV1      = 0x31303031
	bcmediaInfoV2      = 0x32303031
	bcmediaIFrameMin   = 0x63643030
	bcmediaIFrameMax   = 0x63643039
	bcmediaPFrameMin   = 0x63643130
	bcmediaPFrameMax   = 0x63643139
	bcmediaAAC         = 0x62773530
	bcmediaAACV2       = 0x62773531
	bcmediaADPCM       = 0x62773130
	bcmediaADPCMHeader = 0x0100
	bcmediaPadSize     = 8

	// Sanity limits for sizes read from the stream. A 4K keyframe is well
	// below 8 MiB and the AI box header is a few hundred bytes; anything
	// larger is corrupt data, and waiting for it would grow the buffer forever.
	bcmediaMaxPayload = 8 << 20
	bcmediaMaxExtra   = 64 << 10
	// The buffer never needs to hold more than one maximal packet.
	bcmediaMaxBuffered = 24 + bcmediaMaxExtra + bcmediaMaxPayload + bcmediaPadSize
)

// MediaParser incrementally parses the bcmedia byte stream carried by msg_id=3.
type MediaParser struct {
	buf bytes.Buffer
}

// Append adds bytes to the parser and returns every complete media packet found.
func (p *MediaParser) Append(data []byte) ([]MediaPacket, error) {
	if len(data) > 0 {
		_, _ = p.buf.Write(data)
	}

	var out []MediaPacket
	for {
		packet, consumed, ok, err := parseMediaPacket(p.buf.Bytes())
		if err != nil {
			return out, err
		}
		if !ok {
			// What is left is one incomplete packet, bounded by the size
			// limits above; this guards against a path that forgets them.
			if p.buf.Len() > bcmediaMaxBuffered {
				return out, fmt.Errorf("bcmedia buffer exceeds %d bytes", bcmediaMaxBuffered)
			}
			return out, nil
		}

		p.buf.Next(consumed)
		out = append(out, packet)
	}
}

func parseMediaPacket(buf []byte) (MediaPacket, int, bool, error) {
	if len(buf) < 4 {
		return MediaPacket{}, 0, false, nil
	}

	magic := binary.LittleEndian.Uint32(buf[0:4])
	switch {
	case magic == bcmediaInfoV1 || magic == bcmediaInfoV2:
		if len(buf) < 32 {
			return MediaPacket{}, 0, false, nil
		}
		headerSize := binary.LittleEndian.Uint32(buf[4:8])
		if headerSize != 32 {
			return MediaPacket{}, 0, false, fmt.Errorf("unexpected bcmedia info header size %d", headerSize)
		}

		packet := MediaPacket{
			Kind:   MediaPacketInfoV1,
			Width:  binary.LittleEndian.Uint32(buf[8:12]),
			Height: binary.LittleEndian.Uint32(buf[12:16]),
			FPS:    buf[17],
		}
		if magic == bcmediaInfoV2 {
			packet.Kind = MediaPacketInfoV2
		}
		return packet, 32, true, nil

	case magic >= bcmediaIFrameMin && magic <= bcmediaIFrameMax:
		return parseVideoFrame(buf, true)

	case magic >= bcmediaPFrameMin && magic <= bcmediaPFrameMax:
		return parseVideoFrame(buf, false)

	case magic == bcmediaAAC || magic == bcmediaAACV2:
		if len(buf) < 8 {
			return MediaPacket{}, 0, false, nil
		}
		payloadSize := int(binary.LittleEndian.Uint16(buf[4:6]))
		total := 8 + payloadSize + padLen(payloadSize)
		if len(buf) < total {
			return MediaPacket{}, 0, false, nil
		}

		packet := MediaPacket{
			Kind: MediaPacketAAC,
			Data: append([]byte(nil), buf[8:8+payloadSize]...),
		}
		return packet, total, true, nil

	case magic == bcmediaADPCM:
		if len(buf) < 12 {
			return MediaPacket{}, 0, false, nil
		}
		payloadSize := int(binary.LittleEndian.Uint16(buf[4:6]))
		if payloadSize < 4 { // the 4-byte ADPCM header is part of the payload
			return MediaPacket{}, 0, false, fmt.Errorf("adpcm payload size %d too small", payloadSize)
		}
		total := 8 + payloadSize + padLen(payloadSize)
		if len(buf) < total {
			return MediaPacket{}, 0, false, nil
		}
		if binary.LittleEndian.Uint16(buf[8:10]) != bcmediaADPCMHeader {
			return MediaPacket{}, 0, false, fmt.Errorf("unexpected adpcm marker %#x", binary.LittleEndian.Uint16(buf[8:10]))
		}

		blockSize := payloadSize - 4
		packet := MediaPacket{
			Kind: MediaPacketADPCM,
			Data: append([]byte(nil), buf[12:12+blockSize]...),
		}
		return packet, total, true, nil

	default:
		return MediaPacket{}, 0, false, fmt.Errorf("unknown bcmedia magic %#x", magic)
	}
}

func parseVideoFrame(buf []byte, iframe bool) (MediaPacket, int, bool, error) {
	if len(buf) < 24 {
		return MediaPacket{}, 0, false, nil
	}

	codec := string(buf[4:8])
	if codec != "H264" && codec != "H265" {
		return MediaPacket{}, 0, false, fmt.Errorf("unsupported video codec %q", codec)
	}

	rawPayload := binary.LittleEndian.Uint32(buf[8:12])
	rawExtra := binary.LittleEndian.Uint32(buf[12:16])
	// Checked before int conversion: a corrupt size would otherwise make the
	// parser wait for gigabytes that never come.
	if rawPayload > bcmediaMaxPayload {
		return MediaPacket{}, 0, false, fmt.Errorf("video payload size %d too large", rawPayload)
	}
	if rawExtra > bcmediaMaxExtra {
		return MediaPacket{}, 0, false, fmt.Errorf("video additional header size %d too large", rawExtra)
	}
	payloadSize, additionalHeaderSize := int(rawPayload), int(rawExtra)
	microseconds := binary.LittleEndian.Uint32(buf[16:20])

	total := 24 + additionalHeaderSize + payloadSize + padLen(payloadSize)
	if len(buf) < total {
		return MediaPacket{}, 0, false, nil
	}

	pos := 24
	var unixTime *time.Time
	if iframe && additionalHeaderSize >= 4 {
		ts := int64(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		t := time.Unix(ts, 0).UTC()
		unixTime = &t
	}
	var extra []byte // carries the camera's AI boxes, see aibox.go
	if additionalHeaderSize > 0 {
		extra = append([]byte(nil), buf[pos:pos+additionalHeaderSize]...)
	}
	pos += additionalHeaderSize

	packet := MediaPacket{
		Extra:              extra,
		Kind:               MediaPacketPFrame,
		Codec:              codec,
		Data:               append([]byte(nil), buf[pos:pos+payloadSize]...),
		TimestampMicrosecs: microseconds,
		HasTimestamp:       true,
		UnixTime:           unixTime,
	}
	if iframe {
		packet.Kind = MediaPacketIFrame
	}

	return packet, total, true, nil
}

func padLen(size int) int {
	if size%bcmediaPadSize == 0 {
		return 0
	}
	return bcmediaPadSize - (size % bcmediaPadSize)
}
