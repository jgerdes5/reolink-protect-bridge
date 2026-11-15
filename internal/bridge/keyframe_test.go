package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/stretchr/testify/require"
)

type fakeIframer struct {
	mu     sync.Mutex
	sent   []string
	fail   bool // fail the next request (a dropped connection)
	closed bool
}

func (f *fakeIframer) RequestIframe(_ context.Context, ch uint8, stream string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || f.closed {
		f.fail = false
		return errors.New("connection reset")
	}
	f.sent = append(f.sent, stream)
	return nil
}

func (f *fakeIframer) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func TestKeyframer(t *testing.T) {
	var mu sync.Mutex
	var conns []*fakeIframer
	k := newKeyframer(0, func(context.Context) (iframer, error) {
		mu.Lock()
		defer mu.Unlock()
		c := &fakeIframer{}
		conns = append(conns, c)
		return c, nil
	})
	sent := func() (n int) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.mu.Lock()
			n += len(c.sent)
			c.mu.Unlock()
		}
		return n
	}

	// video2 and video3 share the sub stream: one request
	k.request("subStream")
	k.request("subStream")
	k.request("mainStream")
	require.Eventually(t, func() bool { return sent() == 2 }, time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 2, sent())
	mu.Lock()
	require.Len(t, conns, 1) // the connection is kept
	conns[0].mu.Lock()
	conns[0].fail = true
	conns[0].mu.Unlock()
	mu.Unlock()

	// after the gap: a dropped connection is replaced and the request retried
	time.Sleep(keyframeGap)
	k.request("mainStream")
	require.Eventually(t, func() bool { return sent() == 3 }, time.Second, time.Millisecond)
	mu.Lock()
	require.Len(t, conns, 2)
	require.True(t, conns[0].closed)
	mu.Unlock()

	k.close()
	mu.Lock()
	require.True(t, conns[1].closed)
	mu.Unlock()
}

func TestOnFeedback(t *testing.T) {
	var mu sync.Mutex
	var got []string
	b := &Bridge{}
	b.keys = newKeyframer(0, func(context.Context) (iframer, error) {
		return &recordIframer{mu: &mu, got: &got}, nil
	})
	key := unifi.Feedback{Type: unifi.FeedbackKeyframe}

	require.True(t, b.onFeedback("video3", true, key, nil))
	require.True(t, b.onFeedback("video1", false, key, nil)) // transcoded: ffmpeg's keyframes
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	}, time.Second, time.Millisecond)
	mu.Lock()
	require.Equal(t, []string{"subStream"}, got)
	mu.Unlock()

	require.False(t, b.onFeedback("video1", true, unifi.Feedback{Type: unifi.FeedbackInvalidPoint}, nil))
	require.True(t, b.onFeedback("video1", true, unifi.Feedback{Type: unifi.FeedbackEvent, Body: []byte{0, 3}}, nil))

	// RTT probes go to the stream's answering goroutine, not the keyframer
	var probes [][]byte
	body := []byte("probe-0123456789")
	require.True(t, b.onFeedback("video1", true, unifi.Feedback{Type: unifi.FeedbackRTT, Body: body}, func(b []byte) { probes = append(probes, b) }))
	require.Equal(t, [][]byte{body}, probes)
	require.True(t, b.onFeedback("video1", true, unifi.Feedback{Type: unifi.FeedbackRTT, Body: body}, nil))
	mu.Lock()
	require.Len(t, got, 1)
	mu.Unlock()
}

type recordIframer struct {
	mu  *sync.Mutex
	got *[]string
}

func (r *recordIframer) RequestIframe(_ context.Context, _ uint8, stream string) error {
	r.mu.Lock()
	*r.got = append(*r.got, stream)
	r.mu.Unlock()
	return nil
}

func (r *recordIframer) Close() error { return nil }

// The body the D340W firmware parses.
func TestIframeBody(t *testing.T) {
	n, err := baichuan.ParseNode(string(baichuan.IframeBody(0, "subStream")))
	require.NoError(t, err)
	require.Equal(t, "0", n.Value("RequestIframe/channelId"))
	require.Equal(t, "subStream", n.Value("RequestIframe/streamType"))
}
