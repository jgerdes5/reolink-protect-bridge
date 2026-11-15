package bridge

import (
	"context"
	"sync"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// keyframer passes Protect's keyframe requests (feedback type 1, sent when
// live view opens or switches quality) on to the doorbell as IFRAME_REQUEST
// (msg 189), over a control connection kept open between requests.
type keyframer struct {
	dial func(ctx context.Context) (iframer, error)
	ch   uint8

	mu     sync.Mutex
	last   map[string]time.Time // per stream: last request sent
	client iframer
	sendMu sync.Mutex // one request at a time on the connection
	closed bool
}

// keyframeGap merges requests for the same stream that arrive together
// (video2 and video3 share the sub stream). It is short: Protect sends a
// follow-up request about 90 ms after the first, and a G4 answers each one.
const keyframeGap = 50 * time.Millisecond

// iframer is the part of baichuan.Client the keyframer uses.
type iframer interface {
	RequestIframe(ctx context.Context, channel uint8, stream string) error
	Close() error
}

var _ iframer = (*baichuan.Client)(nil)

func newKeyframer(ch uint8, dial func(ctx context.Context) (iframer, error)) *keyframer {
	return &keyframer{dial: dial, ch: ch, last: map[string]time.Time{}}
}

// request asks for a keyframe on stream without blocking the caller.
func (k *keyframer) request(stream string) {
	k.mu.Lock()
	if k.closed || time.Since(k.last[stream]) < keyframeGap {
		k.mu.Unlock()
		return
	}
	k.last[stream] = time.Now()
	k.mu.Unlock()
	go k.send(stream)
}

func (k *keyframer) send(stream string) {
	k.sendMu.Lock()
	defer k.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// An idle control connection may have been dropped by the doorbell: one
	// retry on a fresh one.
	var err error
	for range 2 {
		var c iframer
		if c, err = k.conn(ctx); err != nil {
			break
		}
		if err = c.RequestIframe(ctx, k.ch, stream); err == nil {
			log.Debug().Msgf("[bridge] keyframe requested on %s", stream)
			return
		}
		k.drop(c)
	}
	log.Debug().Err(err).Msgf("[bridge] keyframe request on %s", stream)
}

func (k *keyframer) conn(ctx context.Context) (iframer, error) {
	k.mu.Lock()
	c := k.client
	k.mu.Unlock()
	if c != nil {
		return c, nil
	}
	c, err := k.dial(ctx)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		_ = c.Close()
		return nil, context.Canceled
	}
	k.client = c
	return c, nil
}

func (k *keyframer) drop(c iframer) {
	k.mu.Lock()
	if k.client == c {
		k.client = nil
	}
	k.mu.Unlock()
	_ = c.Close()
}

func (k *keyframer) close() {
	k.mu.Lock()
	k.closed = true
	c := k.client
	k.client = nil
	k.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// streamType is the doorbell stream that feeds a Protect channel.
func streamType(channel string) string {
	if channelIndex(channel) == 0 {
		return "mainStream"
	}
	return "subStream"
}

// onFeedback handles a message Protect sent back on a channel's socket. It
// returns false if the stream should end. RTT probes go to rtt, which must
// not block (it runs on the socket's reading goroutine).
func (b *Bridge) onFeedback(channel string, passthrough bool, f unifi.Feedback, rtt func(body []byte)) bool {
	switch {
	case f.Type == unifi.FeedbackRTT:
		if rtt != nil {
			rtt(f.Body)
		}
	case f.WantsKeyframe():
		if passthrough { // ffmpeg makes its own keyframes every second
			b.keys.request(streamType(channel))
		}
	case f.Type == unifi.FeedbackInvalidPoint:
		log.Warn().Msgf("[bridge] %s: Protect rejected the stream (invalid ingest point)", channel)
		return false
	case f.Type == unifi.FeedbackEvent && len(f.Body) >= 2:
		what := map[byte]string{1: "timestamp went backward", 2: "timestamp jump", 3: "audio/video drift"}[f.Body[1]]
		if what == "" {
			what = "unknown"
		}
		log.Debug().Msgf("[bridge] %s: Protect reports %s", channel, what)
	}
	return true
}
