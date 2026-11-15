package bridge

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/reolink"
	"github.com/pion/rtp"
)

// Talkback: Protect sends ADTS AAC over UDP, or Opus over RTP for HomeKit
// (pkg/unifi/talkback.go). go2rtc backchannels take PCM-family codecs, so
// ffmpeg decodes the audio. A session opens with the first packet and closes
// after talkIdle without audio, releasing the camera's talk channel.

// talkIdle matches the doorbell's own talk session (reolink.TalkIdle): the
// decoder and the backchannel stay up as long as the doorbell keeps listening,
// so speaking again within it starts at once.
const talkIdle = reolink.TalkIdle

type talker struct {
	key    string
	src    *source.Source // the stream with the doorbell's talk channel (main)
	ffmpeg string
	idle   time.Duration // talkIdle if 0

	mu     sync.Mutex
	ch     chan talkPkt  // packets for the running session, nil when idle
	quit   chan struct{} // closed by close
	closed bool
	wg     sync.WaitGroup // the running session
}

// talkPkt is one ADTS frame, or one Opus packet when opus is set.
type talkPkt struct {
	opus bool
	b    []byte
}

func (t *talker) feed(adts []byte)    { t.push(talkPkt{b: adts}) }
func (t *talker) feedOpus(pkt []byte) { t.push(talkPkt{opus: true, b: pkt}) }

func (t *talker) push(p talkPkt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.ch == nil {
		if t.quit == nil {
			t.quit = make(chan struct{})
		}
		// 1-2 s of audio while the session opens (AAC frames are ~46 ms, Opus
		// packets 20 ms): the burst after opening must fit a go2rtc sender's
		// 128-packet buffer
		t.ch = make(chan talkPkt, 48)
		t.wg.Add(1)
		go t.run(t.ch, t.quit)
	}
	select {
	case t.ch <- p:
	default:
	}
}

// run serves talk sessions while audio keeps coming. t.ch stays set until a
// session is fully closed, so audio arriving meanwhile reopens one here
// instead of racing a second session against the closing one on the same
// backchannel.
func (t *talker) run(ch chan talkPkt, quit chan struct{}) {
	defer t.wg.Done()
	var next *talkPkt
	var opened time.Time
	for {
		// at most one new session a second: each one opens the doorbell's
		// talk channel and starts ffmpeg
		if wait := time.Second - time.Since(opened); wait > 0 {
			select {
			case <-quit:
				t.mu.Lock()
				t.ch = nil
				t.mu.Unlock()
				return
			case <-time.After(wait):
			}
		}
		opened = time.Now()
		next = t.session(ch, quit, next)

		t.mu.Lock()
		if (next == nil && len(ch) == 0) || t.closed {
			t.ch = nil
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
	}
}

// session opens a talk session for the format of the first packet and feeds
// it until no audio arrived for talkIdle. A packet in the other format ends
// it and is returned, to start the next session. If the session can't open or
// ffmpeg dies, audio is dropped until the pause, so a broken source is not
// retried for every packet.
func (t *talker) session(ch chan talkPkt, quit chan struct{}, first *talkPkt) *talkPkt {
	if first == nil {
		select {
		case p := <-ch:
			first = &p
		case <-quit:
			return nil
		}
	}
	opus := first.opus
	sess, err := openTalk(t.src, t.ffmpeg, opus)
	if err != nil {
		log.Warn().Err(err).Msgf("[talkback] %s:", t.key)
	} else {
		log.Info().Msgf("[talkback] %s: started (%s)", t.key, sess.format)
	}
	closeSess := func() {
		if sess != nil {
			sess.close()
			sess = nil
			log.Info().Msgf("[talkback] %s: ended", t.key)
		}
	}
	defer closeSess()

	idle := t.idle
	if idle <= 0 {
		idle = talkIdle
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	write := func(p talkPkt) {
		if sess == nil {
			return
		}
		if err := sess.write(p.b); err != nil {
			log.Warn().Err(err).Msgf("[talkback] %s: decoder", t.key)
			closeSess()
		}
	}
	write(*first)
	for {
		select {
		case p := <-ch:
			if p.opus != opus {
				return &p
			}
			write(p)
			timer.Reset(idle)
		case <-timer.C:
			return nil
		case <-quit:
			return nil
		}
	}
}

// close ends a running session and waits for it: the talk channel of a
// stopped bridge is released.
func (t *talker) close() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		if t.quit != nil {
			close(t.quit)
		}
	}
	t.mu.Unlock()
	t.wg.Wait()
}

// talkCodecs are offered in two rounds: producers pick their own first match,
// and the reolink producer lists PCMA 8 kHz first, so 16 kHz PCM is tried alone.
var talkCodecs = [][]*core.Codec{
	{
		{Name: core.CodecPCML, ClockRate: 16000},
		{Name: core.CodecPCM, ClockRate: 16000},
	},
	{
		{Name: core.CodecPCML, ClockRate: 16000},
		{Name: core.CodecPCM, ClockRate: 16000},
		{Name: core.CodecPCMA, ClockRate: 8000},
		{Name: core.CodecPCMU, ClockRate: 8000},
		{Name: core.CodecPCML, ClockRate: 8000},
		{Name: core.CodecPCM, ClockRate: 8000},
	},
}

// talkConsumer is a go2rtc consumer with one recvonly audio media: the stream
// asks it for the track (GetTrack) and hands that to the producer's backchannel.
type talkConsumer struct {
	core.Connection
}

func newTalkConsumer(codecs []*core.Codec) *talkConsumer {
	return &talkConsumer{Connection: core.Connection{
		ID:         core.NewID(),
		FormatName: "unifi/talkback",
		Protocol:   "udp",
		Medias: []*core.Media{{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    codecs,
		}},
	}}
}

// Start is part of core.Producer; the track is fed by the talk session.
func (t *talkConsumer) Start() error { return nil }

func (t *talkConsumer) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	return errors.New("unifi/talkback: no recvonly tracks")
}

type talkSession struct {
	src    *source.Source
	cons   *talkConsumer
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	read   chan struct{} // closed when ffmpeg's output is read to the end
	ogg    *oggWriter    // Opus input: packets wrapped in Ogg for ffmpeg
	format string
}

func (s *talkSession) write(b []byte) error {
	if s.ogg != nil {
		return s.ogg.packet(b)
	}
	_, err := s.stdin.Write(b)
	return err
}

func openTalk(src *source.Source, ffmpegBin string, opus bool) (*talkSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cons *talkConsumer
	var err error
	for _, codecs := range talkCodecs {
		cons = newTalkConsumer(codecs)
		if err = src.AddConsumer(ctx, cons); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if len(cons.Receivers) == 0 {
		src.RemoveConsumer(cons)
		return nil, errors.New("the doorbell stream has no talk channel")
	}
	recv := cons.Receivers[0]

	format, sampleBytes := rawFormat(recv.Codec)
	rate := int(recv.Codec.ClockRate)
	in := "aac"
	if opus {
		in = "ogg"
	}
	cmd := exec.Command(ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0",
		"-f", in, "-i", "pipe:0",
		"-ac", "1", "-ar", strconv.Itoa(rate), "-f", format, "-flush_packets", "1", "pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		src.RemoveConsumer(cons)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		src.RemoveConsumer(cons)
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		src.RemoveConsumer(cons)
		return nil, err
	}
	var ogg *oggWriter
	if opus {
		ogg = &oggWriter{w: stdin, serial: 0x52504231}
		if err = ogg.opusHeaders(); err != nil {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			src.RemoveConsumer(cons)
			return nil, err
		}
	}

	// 20 ms packets, timestamps in samples. A silent first packet opens the
	// doorbell's talk session while ffmpeg is still starting.
	chunk := rate / 50 * sampleBytes
	silence := make([]byte, chunk)
	if format == "alaw" || format == "mulaw" {
		fill := byte(0xd5) // A-law zero
		if format == "mulaw" {
			fill = 0xff
		}
		for i := range silence {
			silence[i] = fill
		}
	}
	recv.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2}, Payload: silence})
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, chunk)
		seq := uint16(1)
		ts := uint32(chunk / sampleBytes)
		for {
			if _, err := io.ReadFull(stdout, buf); err != nil {
				return
			}
			recv.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts},
				Payload: append([]byte(nil), buf...),
			})
			seq++
			ts += uint32(chunk / sampleBytes)
		}
	}()

	name := "AAC"
	if opus {
		name = "Opus"
	}
	return &talkSession{src: src, cons: cons, cmd: cmd, stdin: stdin, read: read, ogg: ogg,
		format: name + " to " + recv.Codec.Name + "/" + strconv.Itoa(rate)}, nil
}

// close lets ffmpeg flush the last audio: stdin closed, output read to the
// end (cmd.Wait closes stdout, so it must come after the reader), then Wait.
func (s *talkSession) close() {
	_ = s.stdin.Close()
	select {
	case <-s.read:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.read
	}
	_ = s.cmd.Wait()
	s.src.RemoveConsumer(s.cons)
	_ = s.cons.Stop()
}

// rawFormat is ffmpeg's raw output format and bytes per sample for a codec.
func rawFormat(codec *core.Codec) (string, int) {
	switch codec.Name {
	case core.CodecPCMA:
		return "alaw", 1
	case core.CodecPCMU:
		return "mulaw", 1
	case core.CodecPCML:
		return "s16le", 2
	default: // core.CodecPCM, big endian
		return "s16be", 2
	}
}
