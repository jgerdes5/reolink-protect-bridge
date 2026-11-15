// Package bridge runs one Reolink doorbell as a UniFi Protect camera: two
// Reolink streams (main, sub) feed Protect's three video channels, snapshots,
// Baichuan push events and talkback.
package bridge

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/magic"
	"github.com/AlexxIT/go2rtc/pkg/mjpeg"
	"github.com/jgerdes5/reolink-protect-bridge/internal/source"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/reolink"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/rs/zerolog"
)

var (
	log     = zerolog.Nop()
	logOnce sync.Once
)

// Doorbell is how to reach the Reolink doorbell (Baichuan, TCP 9000).
type Doorbell struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"` // default 9000
	Username string `json:"username"`
	Password string `json:"password"`
	Channel  int    `json:"channel,omitempty"`
}

// Protect is the UniFi console running Protect.
type Protect struct {
	Host     string `json:"host"`
	Username string `json:"username,omitempty"` // local Protect user, for adoption tokens (and ring "api")
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`   // or a fixed adoption token
	CertPin  string `json:"certPin,omitempty"` // console key for the login (unifi.ProtectClient.CertPin)
}

// Options configure a bridge.
type Options struct {
	Doorbell Doorbell
	Protect  Protect

	Name     string // name sent to Protect
	MAC      string // 12 hex digits, the camera's identity in Protect
	Cert     string // PEM client certificate + key, the camera's TLS identity
	IP       string // address Protect records; default: this host's address towards Protect
	Ring     string // "camera" (default) or "api"
	Events   bool
	Talkback bool
	Settings bool // apply Protect's camera settings to the doorbell

	FFmpeg string // ffmpeg binary, default "ffmpeg"
	// OnCertPin stores the console key learned on the first login.
	OnCertPin func(pin string) `json:"-"`
}

// Bridge is one running doorbell.
type Bridge struct {
	opts  Options
	main  *source.Source
	sub   *source.Source
	proto *unifi.Camera

	mu         sync.Mutex
	info       [2]streamInfo      // main, sub
	codecNotes map[string]string  // channel -> last codec warning
	runners    map[string]*runner // last runner per channel, kept after stop so the next start waits for it
	closed     bool               // Run is tearing down: no new runners
	wg         sync.WaitGroup     // all runners and background loops; Run waits for them
	talk       *talker
	serveFn    func(ctx context.Context, channel, addr, streamName, codec string) // for tests; nil = serve
	events     []Event                                                            // newest last, for the status page
	eventsUp   bool
	settings   *settings
	tracker    *tracker
	keys       *keyframer
	enc        [2]encStream // main, sub encoder (encoder.go)

	tmu        sync.Mutex // transcodes; held while one starts
	transcodes map[string]*sharedTranscode

	omu     sync.Mutex             // Opus encoders (opus.go)
	opus    map[string]*sharedOpus // per source
	opusEnc string                 // ffmpeg's Opus encoder, once known
	opusErr error                  // no Opus encoder: don't ask again

	snapSem  chan struct{}
	snap     []byte
	snapErr  error
	snapTime time.Time
	lastKey  lastKeyframe // the main stream's latest keyframe, for quick snapshots
}

type streamInfo struct {
	width, height, fps int
	codec              string // "h264" or "h265"
	audio              bool
}

type runner struct {
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool // cancelled; still in runners until replaced
}

// Event is a line on the status page.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`
}

var channelNames = [3]string{"video1", "video2", "video3"}

// channelBitrates match bitRateCbrAvg in the ChangeVideoSettings reply.
var channelBitrates = [3]int{6000000, 500000, 300000}

// New prepares a bridge; Run starts it.
//
// The package loggers are set by the first New only: the web UI restarts
// bridges in this process with the same logger, and goroutines of the old
// bridge may still be reading them.
func New(opts Options, logger zerolog.Logger) (*Bridge, error) {
	logOnce.Do(func() {
		log = logger
		reolink.Log = logger
	})
	if opts.FFmpeg == "" {
		opts.FFmpeg = "ffmpeg"
	}
	if opts.Doorbell.Host == "" {
		return nil, errors.New("doorbell host not set")
	}
	if opts.Protect.Host == "" {
		return nil, errors.New("protect host not set")
	}
	if opts.Protect.Token == "" && opts.Protect.Username == "" {
		return nil, errors.New("set a Protect user or an adoption token")
	}

	b := &Bridge{
		opts:    opts,
		runners: map[string]*runner{},
		snapSem: make(chan struct{}, 1),
		tracker: newTracker(),
	}
	b.keys = newKeyframer(uint8(opts.Doorbell.Channel), func(ctx context.Context) (iframer, error) { return b.dialControl(ctx) })
	b.main = source.New("main", b.dialer("main"), logger)
	b.sub = source.New("sub", b.dialer("sub"), logger)

	mac, err := NormalizeMAC(opts.MAC)
	if err != nil {
		return nil, err
	}
	cert, err := unifi.LoadCert(opts.Cert)
	if err != nil {
		return nil, err
	}
	// The address may carry the console's web port ("console:443"): the
	// login uses it as given, the camera protocol (7442) and talkback only
	// the host.
	console := hostOnly(opts.Protect.Host)
	ip := opts.IP
	if ip == "" {
		ip = LocalIP(console)
	}

	tokens := &tokenSource{token: opts.Protect.Token}
	var protect *unifi.ProtectClient
	if opts.Protect.Username != "" {
		protect = &unifi.ProtectClient{Host: opts.Protect.Host, Username: opts.Protect.Username, Password: opts.Protect.Password,
			CertPin: opts.Protect.CertPin, OnPin: opts.OnCertPin}
		tokens.protect = protect
	}

	b.proto = &unifi.Camera{
		Host:     console,
		Name:     opts.Name,
		MAC:      mac,
		IP:       ip,
		Doorbell: true,
		HasAudio: true,
		Identity: unifi.G4Doorbell,
		Cert:     cert,
		Token:    tokens.get,
		Channel:  b.channel,
		Start:    b.start,
		Stop:     b.stop,
		Snapshot: b.Snapshot,
		// with settings sync the doorbell takes Protect's frame rate
		EchoVideo: opts.Settings,
		Log:       logger,
	}
	if opts.Ring == "api" {
		if protect == nil {
			return nil, errors.New(`ring "api" needs the Protect user`)
		}
		b.proto.RingAPI = func(ctx context.Context) error { return protect.Ring(ctx, mac) }
	}
	if opts.Talkback {
		b.talk = &talker{key: opts.Name, src: b.main, ffmpeg: opts.FFmpeg}
		b.proto.Talk = b.talk.feed
		b.proto.TalkOpus = b.talk.feedOpus
	}
	// Always receive Protect's settings: which smart detections are on filters
	// the events; writing them to the doorbell is optional.
	b.settings = newSettings(b, opts.Settings)
	b.proto.Settings = b.settings
	return b, nil
}

// reolinkURL is the go2rtc reolink:// source URL for a stream.
func (b *Bridge) reolinkURL(stream string) string {
	d := b.opts.Doorbell
	host := d.Host
	if d.Port != 0 {
		host = net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	}
	u := url.URL{Scheme: "reolink", User: url.UserPassword(d.Username, d.Password), Host: host, Path: "/" + stream}
	if d.Channel != 0 {
		u.RawQuery = "channel=" + strconv.Itoa(d.Channel)
	}
	return u.String()
}

func (b *Bridge) dialer(stream string) source.DialFunc {
	return func() (source.Producer, error) {
		client, err := reolink.Dial(b.reolinkURL(stream))
		if err != nil {
			return nil, err
		}
		if stream == "main" { // the doorbell sends its AI boxes on the main stream only
			client.OnAIBoxes = b.onBoxes
		}
		if err = client.Probe(); err != nil {
			_ = client.Close()
			return nil, err
		}
		return client, nil
	}
}

// Run serves Protect until ctx ends. It returns once every stream to
// Protect has ended and nothing of this bridge writes to the doorbell any
// more, so a new bridge with the same identity can start right after.
func (b *Bridge) Run(ctx context.Context) {
	defer b.teardown()
	go b.main.Run(ctx)
	go b.sub.Run(ctx)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.keepKeyframe(ctx)
	}()

	// Protect caches resolution and capabilities at adoption: learn them first.
	b.probe(ctx)
	if ctx.Err() != nil {
		return
	}
	if b.opts.Events {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.runEvents(ctx)
		}()
	}
	b.loadEncoder(ctx)
	b.proto.Run(ctx)
}

// teardown stops all channels and waits for them, the event loop, pending
// settings and talkback.
func (b *Bridge) teardown() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	for _, name := range channelNames {
		b.stop(name)
	}
	b.wg.Wait()
	b.settings.close()
	if b.talk != nil {
		b.talk.close()
	}
	b.keys.close()
}

// dialControl opens a Baichuan connection for commands.
func (b *Bridge) dialControl(ctx context.Context) (*baichuan.Client, error) {
	d := b.opts.Doorbell
	return baichuan.Dial(ctx, baichuan.Config{Host: d.Host, Port: d.Port, Username: d.Username, Password: d.Password})
}

// probe learns resolution, frame rate and audio of both streams.
func (b *Bridge) probe(ctx context.Context) {
	for i, src := range []*source.Source{b.main, b.sub} {
		for ctx.Err() == nil {
			info, err := probeSource(ctx, src)
			if err == nil {
				b.mu.Lock()
				b.info[i] = info
				b.mu.Unlock()
				log.Info().Msgf("[bridge] %s stream %s %dx%d %d fps audio=%t", src.Name, info.codec, info.width, info.height, info.fps, info.audio)
				break
			}
			log.Warn().Err(err).Msgf("[bridge] %s stream probe failed, retrying", src.Name)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
	b.mu.Lock()
	b.proto.HasAudio = b.info[0].audio
	b.mu.Unlock()
}

var errNoVideo = errors.New("stream has no H.264 or H.265 video")

// probeSource attaches a consumer for a few seconds: codecs, size from the SPS
// or first keyframe, and the frame rate from counting frames.
func probeSource(ctx context.Context, src *source.Source) (streamInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cons := unifi.NewConsumer(unifi.StreamOptions{})
	if err := src.AddConsumer(ctx, cons); err != nil {
		return streamInfo{}, err
	}
	defer func() {
		src.RemoveConsumer(cons)
		_ = cons.Stop()
	}()

	video, audio := cons.TrackCodecs()
	if video == nil {
		return streamInfo{}, errNoVideo
	}
	info := streamInfo{codec: codecName(video), audio: audio != nil}

	frames := cons.CountFrames(ctx, 3*time.Second)
	if ctx.Err() != nil {
		return streamInfo{}, ctx.Err()
	}
	info.fps = commonFPS(frames / 3)
	info.width, info.height = cons.Size()
	if info.width == 0 && info.codec == "h264" {
		sps, _ := h264.GetParameterSet(video.FmtpLine)
		if s := h264.DecodeSPS(sps); s != nil {
			info.width, info.height = int(s.Width()), int(s.Height())
		}
	}
	return info, nil
}

// codecName is Protect's name for a video codec.
func codecName(c *core.Codec) string {
	if c != nil && c.Name == core.CodecH265 {
		return "h265"
	}
	return "h264"
}

// commonFPS rounds a measured rate to the values Protect offers.
func commonFPS(measured int) int {
	best := 15
	for _, v := range []int{5, 8, 10, 12, 15, 20, 24, 25, 30} {
		if abs(v-measured) < abs(best-measured) {
			best = v
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// channel reports a Protect channel: video1 = main, video2 and video3 = sub.
func (b *Bridge) channel(name string) unifi.Channel {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := min(channelIndex(name), 1)
	info := b.info[i]
	ch := unifi.Channel{Width: info.width, Height: info.height, FPS: info.fps, Codec: info.codec}
	b.encoderChannel(&ch, i)
	return ch
}

func channelIndex(name string) int {
	for i, n := range channelNames {
		if n == name {
			return i
		}
	}
	return 0
}

func (b *Bridge) sourceFor(channel string) *source.Source {
	if channelIndex(channel) == 0 {
		return b.main
	}
	return b.sub
}

func (b *Bridge) start(channel, addr, streamName, codec string) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan struct{})}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		return
	}
	prev := b.runners[channel] // running or stopped but maybe not yet ended
	b.runners[channel] = r
	b.wg.Add(1)
	b.mu.Unlock()
	if prev != nil {
		prev.cancel()
	}

	go func() {
		defer b.wg.Done()
		defer close(r.done)
		if prev != nil {
			<-prev.done // never two streams for one channel
		}
		serve := b.serveFn
		if serve == nil {
			serve = b.serve
		}
		serve(ctx, channel, addr, streamName, codec)
	}()
}

// stop cancels a channel without waiting: it is called from the protocol loop,
// which must keep answering Protect. The runner stays in b.runners, so a
// following start waits until it has really ended.
func (b *Bridge) stop(channel string) {
	b.mu.Lock()
	r := b.runners[channel]
	if r != nil {
		r.stopped = true
	}
	b.mu.Unlock()
	if r != nil {
		r.cancel()
	}
}

// serve keeps one channel streaming to Protect until stopped, re-dialling a
// lost connection to the same destination.
//
// Errors that won't go away by themselves (no video, ffmpeg can't transcode)
// back off up to a minute, and the same error is logged at Error level once.
func (b *Bridge) serve(ctx context.Context, channel, addr, streamName, codec string) {
	var lastErr string
	delay := retryMin
	for {
		err := b.streamOnce(ctx, channel, addr, streamName, codec)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNoVideo) || errors.Is(err, errTranscode) {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				log.Error().Err(err).Msgf("[bridge] %s", channel)
			} else {
				log.Debug().Err(err).Msgf("[bridge] %s, retrying in %s", channel, delay)
			}
		} else {
			lastErr, delay = "", retryMin
			log.Debug().Err(err).Msgf("[bridge] %s ended, retrying", channel)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if lastErr != "" {
			delay = min(delay*2, retryMax)
		}
	}
}

const (
	retryMin = 3 * time.Second
	retryMax = time.Minute
)

func (b *Bridge) streamOnce(ctx context.Context, channel, addr, streamName, codec string) error {
	i := channelIndex(channel)
	src := b.sourceFor(channel)
	info := b.channel(channel)

	cons := unifi.NewConsumer(unifi.StreamOptions{
		StreamName: streamName,
		Channel:    i,
		FPS:        info.FPS,
		Bitrate:    cmp.Or(info.Bitrate, channelBitrates[i]),
		BitrateMin: info.BitrateMin,
		OnKeyframe: func(wait time.Duration) {
			log.Debug().Msgf("[bridge] %s: keyframe %d ms after Protect asked", channel, wait.Milliseconds())
		},
	})
	cons.RemoteAddr = addr
	cons.URL = "unifi:" + channel

	// Source first: while the doorbell is offline Protect sees no empty connections.
	passthrough, err := b.attach(ctx, channel, src, cons, info, codec)
	if err != nil {
		return err
	}
	defer cons.Stop()

	// Protect asks every camera for Opus next to AAC (opus.go). Set up before
	// the first keyframe, which carries its config.
	if _, audio := cons.TrackCodecs(); audio != nil && b.opts.FFmpeg != "" {
		if release, err := b.opusFor(ctx, src, cons); err == nil {
			cons.OnStop(release)
		} else if !errors.Is(err, b.opusErrSnapshot()) {
			log.Debug().Err(err).Msgf("[bridge] %s: no Opus audio", channel)
		}
	}

	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close() // unblocks a pending write first
		_ = cons.Stop()
	})
	defer stop()

	// Protect sends small messages back on this socket (keyframe requests
	// among them); reading them also keeps the TCP window open and notices
	// Protect closing the connection. RTT probes are answered on a goroutine
	// of their own: the answer is a write that may block behind video.
	rtt := make(chan []byte, 1)
	go func() {
		for body := range rtt {
			if ms, ok := cons.RTT(body); ok {
				log.Trace().Msgf("[bridge] %s: round trip %.0f ms", channel, ms)
			}
		}
	}()
	go func() {
		defer close(rtt)
		_ = unifi.ReadFeedback(conn, func(f unifi.Feedback) {
			if f.WantsKeyframe() {
				cons.KeyframeWanted()
			}
			if e := log.Trace(); e.Enabled() {
				e.Msgf("[bridge] %s: feedback type %d % x", channel, f.Type, f.Body)
			} else if f.Type == unifi.FeedbackViewer && len(f.Body) >= 2 {
				v := uint16(f.Body[0])<<8 | uint16(f.Body[1])
				log.Debug().Msgf("[bridge] %s: Protect viewers %d (leaving=%t)", channel, v&0x7fff, v&0x8000 != 0)
			}
			if !b.onFeedback(channel, passthrough, f, func(body []byte) {
				select {
				case rtt <- body:
				default: // one is still being answered: skip this probe
				}
			}) {
				_ = conn.Close()
			}
		})
		_ = cons.Stop()
	}()

	_, err = cons.WriteTo(conn)
	return err
}

var errTranscode = errors.New("transcoding failed")

// attach connects a channel's consumer to its source: directly if the doorbell
// sends the codec Protect asks for, else through ffmpeg (passthrough false).
// The consumer's Stop also releases the source.
func (b *Bridge) attach(ctx context.Context, channel string, src *source.Source, cons *unifi.Consumer, info unifi.Channel, codec string) (passthrough bool, err error) {
	addCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if codec == "" || codec == info.Codec {
		if err := src.AddConsumer(addCtx, cons); err != nil {
			return false, err
		}
		if video, _ := cons.TrackCodecs(); video == nil {
			src.RemoveConsumer(cons)
			_ = cons.Stop()
			return false, errNoVideo
		}
		cons.OnStop(func() { src.RemoveConsumer(cons) })
		return true, nil
	}

	if channelIndex(channel) == 0 {
		// Too large to encode in software: stream it as it is and say why
		// Protect may not show it.
		b.noteCodec(channel, true, fmt.Sprintf("Protect expects %s but the doorbell's main stream is %s: "+
			"set the doorbell to %s or switch Protect's encoding for this camera", upper(codec), upper(info.Codec), upper(codec)))
		if err := src.AddConsumer(addCtx, cons); err != nil {
			return false, err
		}
		cons.OnStop(func() { src.RemoveConsumer(cons) })
		return true, nil
	}

	tr, release, err := b.transcode(addCtx, src, codec, info.FPS)
	if err != nil {
		if !errors.Is(err, errNoVideo) {
			err = fmt.Errorf("%w (%s %s to %s): %w", errTranscode, channel, upper(info.Codec), upper(codec), err)
		}
		return false, err
	}
	r, err := tr.join(cons)
	if err != nil {
		release()
		return false, fmt.Errorf("%w: %w", errTranscode, err)
	}
	tap := newAudioTap(cons)
	_ = src.AddConsumer(addCtx, tap) // fails only without audio
	stopped := make(chan struct{})
	cons.OnStop(func() {
		close(stopped)
		tr.leave(r)
		src.RemoveConsumer(tap)
		release()
	})
	go func() { // ffmpeg ended: so does this stream, and it starts over
		select {
		case <-tr.done:
			_ = cons.Stop()
		case <-stopped:
		}
	}()
	b.noteCodec(channel, false, fmt.Sprintf("transcoding %s from %s to %s with ffmpeg", channel, upper(info.Codec), upper(codec)))
	return false, nil
}

// transcode returns the running transcoder for a source and codec, starting
// one if needed, and a release function; the last release stops it. ffmpeg
// starts outside b.tmu, bounded by ctx: a stalled source must not block the
// other channels. Callers arriving while it starts wait for that start.
func (b *Bridge) transcode(ctx context.Context, src *source.Source, codec string, fps int) (*transcoder, func(), error) {
	key := src.Name + "|" + codec
	b.tmu.Lock()
	st := b.transcodes[key]
	if st != nil && st.dead() { // ffmpeg died or never started: start a new one
		delete(b.transcodes, key)
		st = nil
	}
	starter := st == nil
	if starter {
		st = &sharedTranscode{ready: make(chan struct{})}
		if b.transcodes == nil {
			b.transcodes = map[string]*sharedTranscode{}
		}
		b.transcodes[key] = st
	}
	st.users++
	b.tmu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			b.tmu.Lock()
			defer b.tmu.Unlock()
			if st.users--; st.users > 0 {
				return
			}
			if st.stop != nil {
				st.stop()
			}
			if b.transcodes[key] == st {
				delete(b.transcodes, key)
			}
		})
	}

	if starter {
		tr, stop, err := startTranscode(ctx, src, codec, fps, b.opts.FFmpeg)
		b.tmu.Lock()
		st.tr, st.stop, st.err = tr, stop, err
		close(st.ready)
		b.tmu.Unlock()
	} else {
		select {
		case <-st.ready:
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		}
	}
	if st.err != nil {
		release()
		return nil, nil, st.err
	}
	return st.tr, release, nil
}

// startTranscode attaches a new transcoder to src and starts its ffmpeg; stop
// ends both.
func startTranscode(ctx context.Context, src *source.Source, codec string, fps int, ffmpeg string) (*transcoder, func(), error) {
	tr := newTranscoder(codec, fps)
	if err := src.AddConsumer(ctx, tr); err != nil {
		return nil, nil, err
	}
	detach := func() {
		src.RemoveConsumer(tr)
		_ = tr.Stop()
	}
	if !tr.hasVideo() {
		detach()
		return nil, nil, errNoVideo
	}
	runCtx, cancel := context.WithCancel(context.Background())
	if err := tr.run(ctx, runCtx, ffmpeg); err != nil {
		cancel()
		detach()
		return nil, nil, err
	}
	return tr, func() { cancel(); detach() }, nil
}

// sharedTranscode is one transcoder and its users. tr, stop and err are set
// once ready is closed (under b.tmu).
type sharedTranscode struct {
	ready chan struct{}
	tr    *transcoder
	stop  func()
	err   error
	users int
}

// dead reports a start that failed or an ffmpeg that has exited. Must be
// called with b.tmu held.
func (st *sharedTranscode) dead() bool {
	select {
	case <-st.ready:
	default:
		return false // still starting
	}
	if st.err != nil {
		return true
	}
	select {
	case <-st.tr.done:
		return true
	default:
		return false
	}
}

// noteCodec logs a codec note once per channel and message.
func (b *Bridge) noteCodec(channel string, warn bool, msg string) {
	b.mu.Lock()
	if b.codecNotes == nil {
		b.codecNotes = map[string]string{}
	}
	same := b.codecNotes[channel] == msg
	b.codecNotes[channel] = msg
	b.mu.Unlock()
	switch {
	case same:
	case warn:
		log.Warn().Msgf("[bridge] %s", msg)
	default:
		log.Info().Msgf("[bridge] %s", msg)
	}
}

func upper(codec string) string {
	switch codec {
	case "h265":
		return "H.265"
	case "h264":
		return "H.264"
	}
	return codec
}

// Snapshot returns a JPEG from the main stream (a keyframe through ffmpeg),
// cached for a few seconds, one capture at a time.
func (b *Bridge) Snapshot(ctx context.Context) ([]byte, error) {
	select {
	case b.snapSem <- struct{}{}:
		defer func() { <-b.snapSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if time.Since(b.snapTime) < 5*time.Second {
		return b.snap, b.snapErr
	}
	jpeg, err := b.captureSnapshot(ctx)
	if ctx.Err() == nil { // a caller giving up says nothing about the next one
		b.snap, b.snapErr, b.snapTime = jpeg, err, time.Now()
	}
	return jpeg, err
}

func (b *Bridge) captureSnapshot(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// The latest keyframe saves waiting up to a keyframe interval: Protect's
	// live view shows the snapshot as the poster while a stream starts.
	if frame, codec, ok := b.lastKey.get(2 * time.Second); ok {
		return b.encodeSnapshot(ctx, frame, codec)
	}

	cons := magic.NewKeyframe()
	if err := b.main.AddConsumer(ctx, cons); err != nil {
		return nil, err
	}

	once := &core.OnceBuffer{}
	done := make(chan struct{})
	go func() {
		_, _ = cons.WriteTo(once)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	b.main.RemoveConsumer(cons)
	_ = cons.Stop()
	<-done

	frame := once.Buffer()
	if len(frame) == 0 {
		return nil, errors.New("no keyframe")
	}
	return b.encodeSnapshot(ctx, frame, cons.CodecName())
}

// encodeSnapshot turns a keyframe into a JPEG.
func (b *Bridge) encodeSnapshot(ctx context.Context, frame []byte, codec string) ([]byte, error) {
	if codec == core.CodecJPEG {
		return mjpeg.FixJPEG(frame), nil
	}
	cmd := exec.CommandContext(ctx, b.opts.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-i", "-", "-frames:v", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "mjpeg", "-")
	cmd.Stdin = bytes.NewReader(frame)
	return cmd.Output()
}

// lastKeyframe holds the newest keyframe of a stream (the magic.Keyframe
// consumer writes each one in a single Write).
type lastKeyframe struct {
	mu    sync.Mutex
	frame []byte
	codec string
	at    time.Time
}

func (k *lastKeyframe) Write(p []byte) (int, error) {
	k.mu.Lock()
	k.frame = append(k.frame[:0], p...)
	k.at = time.Now()
	k.mu.Unlock()
	return len(p), nil
}

func (k *lastKeyframe) setCodec(codec string) {
	k.mu.Lock()
	k.codec, k.frame, k.at = codec, k.frame[:0], time.Time{}
	k.mu.Unlock()
}

// get returns a copy of the keyframe if it is newer than maxAge.
func (k *lastKeyframe) get(maxAge time.Duration) ([]byte, string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.frame) == 0 || time.Since(k.at) > maxAge {
		return nil, "", false
	}
	return append([]byte(nil), k.frame...), k.codec, true
}

// keepKeyframe keeps lastKey filled from the main stream until ctx ends.
func (b *Bridge) keepKeyframe(ctx context.Context) {
	for ctx.Err() == nil {
		cons := magic.NewKeyframe()
		if err := b.main.AddConsumer(ctx, cons); err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			continue
		}
		b.lastKey.setCodec(cons.CodecName())
		stop := context.AfterFunc(ctx, func() {
			b.main.RemoveConsumer(cons)
			_ = cons.Stop()
		})
		_, _ = cons.WriteTo(&b.lastKey)
		stop()
		b.main.RemoveConsumer(cons)
		_ = cons.Stop()
		select { // the stream ended: don't spin while it reconnects
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// onBoxes passes the doorbell's AI boxes, with zone and line results, to
// Protect (reading goroutine of the main stream: must be quick).
func (b *Bridge) onBoxes(boxes []baichuan.AIBox, w, h int) {
	b.proto.Detections(b.tracker.frame(boxes, w, h))
}

// addEvent records an event for the status page.
func (b *Bridge) addEvent(kind string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, Event{Time: time.Now(), Kind: kind})
	if len(b.events) > 50 {
		b.events = b.events[len(b.events)-50:]
	}
}

// Status is shown on the web page.
type Status struct {
	Doorbell struct {
		Main   SourceStatus `json:"main"`
		Sub    SourceStatus `json:"sub"`
		Events bool         `json:"events"`
	} `json:"doorbell"`
	Protect  unifi.Status `json:"protect"`
	Channels []string     `json:"channels"` // streaming to Protect
	Recent   []Event      `json:"recent"`
}

type SourceStatus struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since,omitzero"`
	Error     string    `json:"error,omitempty"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	FPS       int       `json:"fps,omitempty"`
	Codec     string    `json:"codec,omitempty"` // h264 | h265
	Audio     bool      `json:"audio"`
}

func (b *Bridge) Status() Status {
	var st Status
	for i, src := range []*source.Source{b.main, b.sub} {
		ok, since, err := src.Status()
		ss := SourceStatus{Connected: ok, Since: since}
		if err != nil {
			ss.Error = err.Error()
		}
		b.mu.Lock()
		ss.Width, ss.Height, ss.FPS, ss.Audio = b.info[i].width, b.info[i].height, b.info[i].fps, b.info[i].audio
		ss.Codec = b.info[i].codec
		b.mu.Unlock()
		if i == 0 {
			st.Doorbell.Main = ss
		} else {
			st.Doorbell.Sub = ss
		}
	}
	st.Protect = b.proto.Status()
	st.Channels = b.streaming()
	b.mu.Lock()
	st.Recent = append([]Event(nil), b.events...)
	st.Doorbell.Events = b.eventsUp
	b.mu.Unlock()
	return st
}

// streaming lists the channels Protect has asked for and not stopped.
func (b *Bridge) streaming() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, name := range channelNames {
		if r := b.runners[name]; r != nil && !r.stopped {
			out = append(out, name)
		}
	}
	return out
}

// NormalizeMAC returns 12 upper-case hex digits.
func NormalizeMAC(mac string) (string, error) {
	mac = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
	if b, err := hex.DecodeString(mac); err != nil || len(b) != 6 {
		return "", errors.New("bad mac: " + mac)
	}
	return mac, nil
}

// NewMAC makes a random locally administered MAC (02:...), which never
// collides with real hardware.
func NewMAC(seed string) string {
	sum := sha256.Sum256([]byte("reolink-protect-bridge-" + seed))
	return "02" + strings.ToUpper(hex.EncodeToString(sum[:5]))
}

// hostOnly drops a port from "host:port" or "[v6]:port".
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

// LocalIP is the address this host uses to reach host.
func LocalIP(host string) string {
	conn, err := net.Dial("udp", net.JoinHostPort(host, "7442"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// tokenSource hands out adoption tokens: a fixed one, or one fetched with the
// Protect login, reused until Protect refuses it.
type tokenSource struct {
	mu      sync.Mutex
	token   string
	protect *unifi.ProtectClient
}

func (t *tokenSource) get(ctx context.Context, fresh bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.protect == nil {
		return t.token, nil
	}
	if t.token != "" && !fresh {
		return t.token, nil
	}
	token, err := t.protect.AdoptionToken(ctx)
	if err != nil {
		return "", err
	}
	t.token = token
	return token, nil
}
