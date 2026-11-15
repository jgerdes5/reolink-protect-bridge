package bridge

import (
	"encoding/binary"
	"io"
)

// oggWriter wraps Opus packets in an Ogg stream (RFC 3533, RFC 7845), one
// packet per page, so ffmpeg can decode them.
type oggWriter struct {
	w       io.Writer
	serial  uint32
	seq     uint32
	granule uint64 // 48 kHz samples
	buf     []byte
}

func (o *oggWriter) opusHeaders() error {
	head := []byte("OpusHead")
	head = append(head, 1, 1)                        // version, mono
	head = binary.LittleEndian.AppendUint16(head, 0) // pre-skip
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = binary.LittleEndian.AppendUint16(head, 0) // gain
	head = append(head, 0)                           // mapping family
	if err := o.page(0x02, 0, head); err != nil {
		return err
	}
	tags := []byte("OpusTags")
	tags = binary.LittleEndian.AppendUint32(tags, 0) // vendor
	tags = binary.LittleEndian.AppendUint32(tags, 0) // comments
	return o.page(0, 0, tags)
}

func (o *oggWriter) packet(pkt []byte) error {
	if len(pkt) == 0 || len(pkt) > 255*254 {
		return nil
	}
	o.granule += uint64(opusSamples(pkt))
	return o.page(0, o.granule, pkt)
}

func (o *oggWriter) page(flags byte, granule uint64, body []byte) error {
	nseg := len(body)/255 + 1
	b := append(o.buf[:0], "OggS"...)
	b = append(b, 0, flags)
	b = binary.LittleEndian.AppendUint64(b, granule)
	b = binary.LittleEndian.AppendUint32(b, o.serial)
	b = binary.LittleEndian.AppendUint32(b, o.seq)
	b = append(b, 0, 0, 0, 0, byte(nseg)) // CRC filled in below
	for i := 0; i < nseg-1; i++ {
		b = append(b, 255)
	}
	b = append(b, byte(len(body)%255))
	b = append(b, body...)
	binary.LittleEndian.PutUint32(b[22:], oggCRC(b))
	o.seq++
	o.buf = b
	_, err := o.w.Write(b)
	return err
}

var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return
}()

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}
