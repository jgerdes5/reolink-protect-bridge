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
	"github.com/pion/rtp"
)

// Talkback: Protect sends ADTS AAC (22.05 kHz mono) over UDP, one frame per
// datagram (see pkg/unifi/talkback.go). go2rtc backchannels take PCM-family
// codecs (the reolink producer: PCMA/PCMU 8 kHz, PCM 16/8 kHz, which it turns
// into the doorbell's ADPCM), so ffmpeg decodes the AAC. A session opens with
// the first frame and closes after talkIdle without audio, which also releases
// the camera's talk channel.

const talkIdle = 2 * time.Second

type talker struct {
	key    string
	src    *source.Source // the stream with the doorbell's talk channel (main)
	ffmpeg string

	mu sync.Mutex
	ch chan []byte // frames for the running session, nil when idle
}

func (t *talker) feed(adts []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ch == nil {
		// ~2 s of audio while the session opens: the burst after opening must
		// fit a go2rtc sender's 128-packet buffer
		t.ch = make(chan []byte, 48)
		go t.run(t.ch)
	}
	select {
	case t.ch <- adts:
	default:
	}
}

// run serves talk sessions while audio keeps coming. t.ch stays set until a
// session is fully closed, so audio arriving meanwhile reopens one here
// instead of racing a second session against the closing one on the same
// backchannel.
func (t *talker) run(ch chan []byte) {
	for {
		t.session(ch)

		t.mu.Lock()
		if len(ch) == 0 {
			t.ch = nil
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
	}
}

// session opens a talk session and feeds it until no audio arrived for
// talkIdle. If the session can't open or ffmpeg dies, audio is dropped until
// the pause, so a broken source is not retried for every frame.
func (t *talker) session(ch chan []byte) {
	sess, err := openTalk(t.src, t.ffmpeg)
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

	timer := time.NewTimer(talkIdle)
	defer timer.Stop()
	for {
		select {
		case b := <-ch:
			if sess != nil {
				if _, err := sess.stdin.Write(b); err != nil {
					log.Warn().Err(err).Msgf("[talkback] %s: decoder", t.key)
					closeSess()
				}
			}
			timer.Reset(talkIdle)
		case <-timer.C:
			return
		}
	}
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
	format string
}

func openTalk(src *source.Source, ffmpegBin string) (*talkSession, error) {
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
	cmd := exec.Command(ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0",
		"-f", "aac", "-i", "pipe:0",
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

	// 20 ms packets, timestamps in samples
	go func() {
		chunk := rate / 50 * sampleBytes
		buf := make([]byte, chunk)
		var seq uint16
		var ts uint32
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

	return &talkSession{src: src, cons: cons, cmd: cmd, stdin: stdin,
		format: recv.Codec.Name + "/" + strconv.Itoa(rate)}, nil
}

func (s *talkSession) close() {
	_ = s.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
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
