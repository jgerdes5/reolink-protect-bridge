package bridge

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// slowServe is a channel stream that takes a while to end after its cancel,
// like a stream closing its socket and ffmpeg.
type slowServe struct {
	active, max, runs atomic.Int32
}

func (s *slowServe) serve(ctx context.Context, _, _, _, _ string) {
	n := s.active.Add(1)
	for {
		m := s.max.Load()
		if n <= m || s.max.CompareAndSwap(m, n) {
			break
		}
	}
	s.runs.Add(1)
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	s.active.Add(-1)
}

func testBridge(s *slowServe) *Bridge {
	b := &Bridge{runners: map[string]*runner{}, serveFn: s.serve}
	b.keys = newKeyframer(0, func(context.Context) (iframer, error) { return nil, errors.New("no doorbell") })
	return b
}

// A restart of a channel waits until the stopped stream has ended: Protect
// never gets two streams for one channel.
func TestStartAfterStopWaits(t *testing.T) {
	s := &slowServe{}
	b := testBridge(s)

	b.start("video1", "a", "s", "")
	require.Eventually(t, func() bool { return s.active.Load() == 1 }, time.Second, time.Millisecond)
	b.stop("video1")
	require.Empty(t, b.streaming()) // stopped, though still ending
	b.start("video1", "a", "s", "")
	b.start("video1", "a", "s", "") // replaces the one still waiting
	require.Equal(t, []string{"video1"}, b.streaming())

	require.Eventually(t, func() bool { return s.runs.Load() >= 2 }, time.Second, time.Millisecond)
	b.teardown()
	require.Equal(t, int32(1), s.max.Load())
	require.Equal(t, int32(0), s.active.Load())
}

// Run's teardown returns only when all streams have ended, and no stream
// starts after it.
func TestTeardownWaits(t *testing.T) {
	s := &slowServe{}
	b := testBridge(s)
	for _, ch := range channelNames {
		b.start(ch, "a", "s", "")
	}
	require.Eventually(t, func() bool { return s.active.Load() == 3 }, time.Second, time.Millisecond)
	b.teardown()
	require.Equal(t, int32(0), s.active.Load())

	b.start("video1", "a", "s", "")
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(3), s.runs.Load())
}

// A cancelled capture is not cached for the next caller.
func TestSnapshotDoesNotCacheCancel(t *testing.T) {
	b := &Bridge{snapSem: make(chan struct{}, 1), main: source.New("main", func() (source.Producer, error) {
		return nil, errors.New("offline")
	}, zerolog.Nop())}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Snapshot(ctx)
	require.Error(t, err)
	require.True(t, b.snapTime.IsZero())
}

// silentVideo is a source with an H.264 track that never sends a frame.
type silentVideo struct {
	core.Connection
	done chan struct{}
}

func (s *silentVideo) AddTrack(*core.Media, *core.Codec, *core.Receiver) error {
	return errors.New("no backchannel")
}
func (s *silentVideo) Start() error { <-s.done; return nil }
func (s *silentVideo) Stop() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

// A stalled source: ffmpeg never produces output. Starting the transcoder
// ends with the caller's context, does not hold b.tmu meanwhile, and leaves
// no ffmpeg behind.
func TestTranscodeStalledSource(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := source.New("sub", func() (source.Producer, error) {
		p := &silentVideo{done: make(chan struct{})}
		p.Medias = []*core.Media{{Kind: core.KindVideo, Direction: core.DirectionRecvonly,
			Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}}}}
		return p, nil
	}, zerolog.Nop())
	srcCtx, srcCancel := context.WithCancel(context.Background())
	defer srcCancel()
	go src.Run(srcCtx)

	b := &Bridge{opts: Options{FFmpeg: ffmpeg}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = b.transcode(ctx, src, "h264", 15)
		}()
	}
	time.Sleep(200 * time.Millisecond)
	require.True(t, b.tmu.TryLock(), "b.tmu held while ffmpeg starts")
	b.tmu.Unlock()

	start := time.Now()
	wg.Wait()
	require.Less(t, time.Since(start), 3*time.Second)
	for _, err := range errs {
		require.Error(t, err)
	}
	b.tmu.Lock()
	require.Empty(t, b.transcodes)
	b.tmu.Unlock()
}

func TestAlarmStateSkipsDisabledTypes(t *testing.T) {
	st := alarmState{enabled: func(o string) bool { return o != "vehicle" }}
	sink := &sinkLog{}
	ctx := context.Background()
	st.apply(ctx, baichuan.AlarmEvent{AI: []string{"vehicle"}}, sink)
	st.apply(ctx, baichuan.AlarmEvent{AI: []string{"vehicle", "people"}}, sink) // the person is not hidden
	st.apply(ctx, baichuan.AlarmEvent{AI: []string{"vehicle"}}, sink)           // only the vehicle left: ended
	require.Equal(t, []string{"smart person", "smart "}, sink.calls)
}

// failDial is a doorbell that can't be reached; it records how many dials
// overlap.
type failDial struct {
	active, max, calls atomic.Int32
	release            chan struct{}
	during             func() // runs inside the first dial
}

func (f *failDial) dial(context.Context) (*baichuan.Client, error) {
	n := f.active.Add(1)
	if n > f.max.Load() {
		f.max.Store(n)
	}
	if f.calls.Add(1) == 1 && f.during != nil {
		f.during()
	}
	<-f.release
	f.active.Add(-1)
	return nil, errors.New("unreachable")
}

// Flushes run one at a time: two at once would GET before the other's SET.
func TestSettingsFlushSerialized(t *testing.T) {
	f := &failDial{release: make(chan struct{})}
	s := newSettings(nil, false) // apply off: no timer, flush called by hand
	s.dial = f.dial
	defer s.close()

	s.ISP(unifi.ISP{Brightness: iptr(10)})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.flush()
		}()
	}
	require.Eventually(t, func() bool { return f.calls.Load() >= 1 }, time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(f.release)
	wg.Wait()
	require.Equal(t, int32(1), f.max.Load())
	require.Equal(t, int32(2), f.calls.Load()) // the second got the values back
}

// A failed dial keeps the values: merged with what arrived meanwhile, the
// newer winning.
func TestSettingsPutBack(t *testing.T) {
	f := &failDial{release: make(chan struct{})}
	close(f.release)
	s := newSettings(nil, false)
	s.dial = f.dial
	defer s.close()

	s.ISP(unifi.ISP{Brightness: iptr(10), Hue: iptr(5)})
	s.SoundLed(unifi.SoundLed{SpeakerVolume: iptr(50)})
	f.during = func() {
		s.ISP(unifi.ISP{Brightness: iptr(30), Contrast: iptr(20)})
	}
	s.flush()

	s.mu.Lock()
	defer s.mu.Unlock()
	require.Equal(t, 30, *s.isp.Brightness)
	require.Equal(t, 20, *s.isp.Contrast)
	require.Equal(t, 5, *s.isp.Hue)
	require.Equal(t, 50, *s.sound.SpeakerVolume)
}

// Partial messages add up instead of replacing what is pending.
func TestSettingsMerge(t *testing.T) {
	s := newSettings(nil, false)
	defer s.close()
	s.ISP(unifi.ISP{Brightness: iptr(10), Masks: []unifi.Mask{{ID: 1}}})
	s.ISP(unifi.ISP{Contrast: iptr(20)})
	s.ISP(unifi.ISP{Brightness: iptr(40)})
	s.SoundLed(unifi.SoundLed{SpeakerVolume: iptr(50)})
	s.SoundLed(unifi.SoundLed{RingVolume: iptr(0)})

	require.Equal(t, 40, *s.isp.Brightness)
	require.Equal(t, 20, *s.isp.Contrast)
	require.Len(t, s.isp.Masks, 1)
	require.Equal(t, 50, *s.sound.SpeakerVolume)
	require.Equal(t, 0, *s.sound.RingVolume)

	s.ISP(unifi.ISP{Masks: []unifi.Mask{}}) // all zones removed
	require.Empty(t, s.isp.Masks)
	require.NotNil(t, s.isp.Masks)
}

// close stops the timer: nothing of a stopped bridge writes to the doorbell.
func TestSettingsClose(t *testing.T) {
	f := &failDial{release: make(chan struct{})}
	close(f.release)
	s := newSettings(nil, true)
	s.dial = f.dial
	s.ISP(unifi.ISP{Brightness: iptr(10)})
	s.close()
	s.ISP(unifi.ISP{Brightness: iptr(20)})
	time.Sleep(settleDelay + 200*time.Millisecond)
	require.Equal(t, int32(0), f.calls.Load())
}

// Debounce: a single change goes out after settleQuiet; a slider dragged
// without pause still goes out within settleMax of its first change.
func TestSettingsDebounce(t *testing.T) {
	f := &failDial{release: make(chan struct{})}
	close(f.release)
	s := newSettings(nil, true)
	s.dial = f.dial
	defer s.close()

	start := time.Now()
	s.ISP(unifi.ISP{Brightness: iptr(10)})
	require.Eventually(t, func() bool { return f.calls.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.Less(t, time.Since(start), settleQuiet+150*time.Millisecond)

	s2 := newSettings(nil, true)
	f2 := &failDial{release: make(chan struct{})}
	close(f2.release)
	s2.dial = f2.dial
	defer s2.close()
	start = time.Now()
	var first time.Duration
	for i := 0; time.Since(start) < 2*settleMax; i++ {
		s2.ISP(unifi.ISP{Brightness: iptr(i % 100)})
		if first == 0 && f2.calls.Load() > 0 {
			first = time.Since(start)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NotZero(t, first, "never flushed while changes kept coming")
	require.Less(t, first, settleMax+200*time.Millisecond)
}

func TestLastKeyframe(t *testing.T) {
	var k lastKeyframe
	_, _, ok := k.get(time.Second)
	require.False(t, ok)

	k.setCodec("H264")
	_, _ = k.Write([]byte{1, 2, 3})
	frame, codec, ok := k.get(time.Second)
	require.True(t, ok)
	require.Equal(t, []byte{1, 2, 3}, frame)
	require.Equal(t, "H264", codec)

	frame[0] = 9 // a copy
	_, _ = k.Write([]byte{4})
	frame, _, _ = k.get(time.Second)
	require.Equal(t, []byte{4}, frame)

	k.at = time.Now().Add(-3 * time.Second)
	_, _, ok = k.get(2 * time.Second)
	require.False(t, ok)
}

func TestReolinkURL(t *testing.T) {
	b := &Bridge{opts: Options{Doorbell: Doorbell{Host: "fd00::10", Username: "admin", Password: "p"}}}
	u, err := url.Parse(b.reolinkURL("main"))
	require.NoError(t, err)
	require.Equal(t, "fd00::10", u.Hostname())
	require.Equal(t, "9000", u.Port())

	b.opts.Doorbell = Doorbell{Host: "192.0.2.5", Port: 9001, Channel: 2}
	u, err = url.Parse(b.reolinkURL("sub"))
	require.NoError(t, err)
	require.Equal(t, "192.0.2.5:9001", u.Host)
	require.Equal(t, "channel=2", u.RawQuery)
}
