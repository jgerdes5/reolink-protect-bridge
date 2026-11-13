// Package source keeps one Reolink stream (main or sub) connected and hands its
// tracks to consumers: the UniFi FLV consumers, snapshots and talkback. It is a
// small, single-producer version of go2rtc's internal/streams: on a reconnect
// the new producer's tracks replace the old ones under the running consumers,
// and backchannel tracks are attached again.
package source

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/rs/zerolog"
)

// Producer is what a source runs: in production a *reolink.Client.
type Producer interface {
	core.Producer
	AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error
}

// DialFunc connects and probes a producer.
type DialFunc func() (Producer, error)

type Source struct {
	Name string
	Dial DialFunc
	Log  zerolog.Logger

	mu        sync.Mutex
	prod      Producer
	receivers []*core.Receiver // recvonly tracks handed to consumers
	backs     []back           // consumer tracks attached to the producer's backchannel
	connected chan struct{}    // closed while a producer is up
	err       error            // last dial or stream error
	since     time.Time        // connected since
}

type back struct {
	cons  core.Consumer
	track *core.Receiver
}

func New(name string, dial DialFunc, log zerolog.Logger) *Source {
	return &Source{Name: name, Dial: dial, Log: log, connected: make(chan struct{})}
}

// Run keeps the producer connected until ctx ends.
func (s *Source) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		prod, err := s.Dial()
		if err != nil {
			s.setErr(err)
			s.Log.Warn().Err(err).Msgf("[source] %s: connect failed, retrying in %s", s.Name, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second

		s.attach(prod)
		s.Log.Info().Msgf("[source] %s: connected", s.Name)

		done := make(chan error, 1)
		go func() { done <- prod.Start() }()
		select {
		case err = <-done:
		case <-ctx.Done():
			_ = prod.Stop()
			<-done
		}
		_ = prod.Stop()
		s.detach(err)
		if ctx.Err() == nil {
			s.Log.Warn().Err(err).Msgf("[source] %s: disconnected", s.Name)
		}
	}
}

// attach makes prod the current producer: existing consumer tracks move over
// to its tracks, and backchannel tracks are added to it.
func (s *Source) attach(prod Producer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, old := range s.receivers {
		if track := trackFor(prod, old.Codec); track != nil {
			old.Replace(track)
			s.receivers[i] = track
		}
	}
	for _, b := range s.backs {
		if media, codec := sendFor(prod, b.track.Codec); media != nil {
			if err := prod.AddTrack(media, codec, b.track); err != nil {
				s.Log.Debug().Err(err).Msgf("[source] %s: backchannel", s.Name)
			}
		}
	}

	s.prod = prod
	s.err = nil
	s.since = time.Now()
	close(s.connected)
}

func (s *Source) detach(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prod = nil
	s.err = err
	s.connected = make(chan struct{})
}

func (s *Source) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// Status reports whether the source is connected, since when, and the last error.
func (s *Source) Status() (connected bool, since time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prod != nil, s.since, s.err
}

// Medias returns the connected producer's medias (nil while disconnected).
func (s *Source) Medias() []*core.Media {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prod == nil {
		return nil
	}
	return s.prod.GetMedias()
}

// AddConsumer waits for the source to be connected (bounded by ctx) and
// connects the consumer's medias to the producer, both directions.
func (s *Source) AddConsumer(ctx context.Context, cons core.Consumer) error {
	s.mu.Lock()
	connected := s.connected
	s.mu.Unlock()
	select {
	case <-connected:
	case <-ctx.Done():
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("%s not connected: %w", s.Name, err)
		}
		return fmt.Errorf("%s not connected", s.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prod == nil {
		return errors.New(s.Name + " disconnected")
	}

	matched := false
	for _, consMedia := range cons.GetMedias() {
	medias:
		for _, prodMedia := range s.prod.GetMedias() {
			prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
			if prodCodec == nil {
				continue
			}
			switch prodMedia.Direction {
			case core.DirectionRecvonly:
				track, err := s.track(prodMedia, prodCodec)
				if err != nil {
					return err
				}
				if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
					return err
				}
			case core.DirectionSendonly:
				p, ok := cons.(core.Producer)
				if !ok {
					continue
				}
				track, err := p.GetTrack(consMedia, consCodec)
				if err != nil {
					return err
				}
				if err = s.prod.AddTrack(prodMedia, prodCodec, track); err != nil {
					return err
				}
				s.backs = append(s.backs, back{cons: cons, track: track})
			}
			matched = true
			break medias
		}
	}
	if !matched {
		return fmt.Errorf("%s: no matching media for %s", s.Name, consumerName(cons))
	}
	return nil
}

// RemoveConsumer forgets a consumer's backchannel tracks; its own Stop closes
// the senders it got from the producer's tracks.
func (s *Source) RemoveConsumer(cons core.Consumer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backs := s.backs[:0]
	for _, b := range s.backs {
		if b.cons != cons {
			backs = append(backs, b)
		}
	}
	s.backs = backs
}

// track returns the producer's track for a codec, shared by all consumers.
func (s *Source) track(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	for _, r := range s.receivers {
		if r.Codec == codec {
			return r, nil
		}
	}
	track, err := s.prod.GetTrack(media, codec)
	if err != nil {
		return nil, err
	}
	s.receivers = append(s.receivers, track)
	return track, nil
}

func trackFor(prod Producer, codec *core.Codec) *core.Receiver {
	for _, media := range prod.GetMedias() {
		if media.Direction != core.DirectionRecvonly {
			continue
		}
		if c := media.MatchCodec(codec); c != nil {
			if track, err := prod.GetTrack(media, c); err == nil {
				return track
			}
		}
	}
	return nil
}

func sendFor(prod Producer, codec *core.Codec) (*core.Media, *core.Codec) {
	for _, media := range prod.GetMedias() {
		if media.Direction != core.DirectionSendonly {
			continue
		}
		if c := media.MatchCodec(codec); c != nil {
			return media, c
		}
	}
	return nil, nil
}

func consumerName(cons core.Consumer) string {
	if info, ok := cons.(interface{ GetSource() string }); ok {
		return info.GetSource()
	}
	return fmt.Sprintf("%T", cons)
}
