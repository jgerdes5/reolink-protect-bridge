package source

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fakeProducer has one video track and a backchannel; Start blocks until
// Stop or until the test drops it.
type fakeProducer struct {
	core.Connection
	drop  chan struct{}
	stop  sync.Once
	backs atomic.Int32 // backchannel tracks attached
}

func newFakeProducer() *fakeProducer {
	p := &fakeProducer{drop: make(chan struct{})}
	p.Medias = []*core.Media{
		{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000}}},
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000}}},
	}
	return p
}

func (p *fakeProducer) Start() error {
	<-p.drop
	return errors.New("dropped")
}

func (p *fakeProducer) Stop() error {
	p.stop.Do(func() { close(p.drop) })
	return nil
}

func (p *fakeProducer) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	p.backs.Add(1)
	return nil
}

// recorder counts video packets and offers a backchannel track.
type recorder struct {
	core.Connection
	packets atomic.Int32
}

func newRecorder() *recorder {
	r := &recorder{}
	r.Medias = []*core.Media{
		{Kind: core.KindVideo, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecH264}}},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{{Name: core.CodecPCMA, ClockRate: 8000}}},
	}
	return r
}

func (r *recorder) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	s := core.NewSender(media, track.Codec)
	s.Handler = func(*core.Packet) { r.packets.Add(1) }
	s.HandleRTP(track)
	r.Senders = append(r.Senders, s)
	return nil
}

func (r *recorder) Start() error { return nil }

func TestSourceReconnect(t *testing.T) {
	var mu sync.Mutex
	var prods []*fakeProducer
	src := New("main", func() (Producer, error) {
		p := newFakeProducer()
		mu.Lock()
		prods = append(prods, p)
		mu.Unlock()
		return p, nil
	}, zerolog.Nop())
	current := func() *fakeProducer {
		mu.Lock()
		defer mu.Unlock()
		return prods[len(prods)-1]
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	rec := newRecorder()
	addCtx, addCancel := context.WithTimeout(ctx, time.Second)
	defer addCancel()
	require.NoError(t, src.AddConsumer(addCtx, rec))
	require.Equal(t, int32(1), current().backs.Load()) // talkback attached

	send := func() {
		for _, r := range current().Receivers {
			r.WriteRTP(&rtp.Packet{Payload: []byte{1}})
		}
	}
	send()
	require.Eventually(t, func() bool { return rec.packets.Load() == 1 }, time.Second, time.Millisecond)

	// the doorbell drops: the source dials again, the consumer keeps receiving
	first := current()
	_ = first.Stop()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		ok, _, _ := src.Status()
		return len(prods) == 2 && ok
	}, 3*time.Second, 5*time.Millisecond)
	require.Equal(t, int32(1), current().backs.Load()) // talkback re-attached to the new producer

	send()
	require.Eventually(t, func() bool { return rec.packets.Load() == 2 }, time.Second, time.Millisecond)

	// a removed consumer's backchannel is not attached on the next reconnect
	src.RemoveConsumer(rec)
	_ = current().Stop()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		ok, _, _ := src.Status()
		return len(prods) == 3 && ok
	}, 3*time.Second, 5*time.Millisecond)
	require.Equal(t, int32(0), current().backs.Load())
}

func TestSourceNotConnected(t *testing.T) {
	src := New("main", func() (Producer, error) { return nil, errors.New("offline") }, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	addCtx, addCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer addCancel()
	err := src.AddConsumer(addCtx, newRecorder())
	require.ErrorContains(t, err, "offline")
}
