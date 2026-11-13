// Package bridge runs one Reolink doorbell as a UniFi Protect camera: two
// Reolink streams (main, sub) feed Protect's three video channels, snapshots,
// Baichuan push events and talkback.
package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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
	"github.com/jgerdes5/reolink-protect-bridge/pkg/reolink"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/rs/zerolog"
)

var log = zerolog.Nop()

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
	Token    string `json:"token,omitempty"` // or a fixed adoption token
}

// Options configure a bridge.
type Options struct {
	Doorbell Doorbell
	Protect  Protect

	Name     string // name sent to Protect
	Model    string // identity preset, see unifi.Identities
	MAC      string // 12 hex digits, the camera's identity in Protect
	Cert     string // PEM client certificate + key, the camera's TLS identity
	IP       string // address Protect records; default: this host's address towards Protect
	Ring     string // "camera" (default) or "api"
	Events   bool
	Talkback bool
	Settings bool // apply Protect's camera settings to the doorbell

	FFmpeg string // ffmpeg binary, default "ffmpeg"
}

// Bridge is one running doorbell.
type Bridge struct {
	opts  Options
	main  *source.Source
	sub   *source.Source
	proto *unifi.Camera

	mu       sync.Mutex
	info     [2]streamInfo // main, sub
	runners  map[string]*runner
	events   []Event // newest last, for the status page
	eventsUp bool

	snapSem  chan struct{}
	snap     []byte
	snapErr  error
	snapTime time.Time
}

type streamInfo struct {
	width, height, fps int
	audio              bool
}

type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
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
func New(opts Options, logger zerolog.Logger) (*Bridge, error) {
	log = logger
	reolink.Log = logger
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
	}
	b.main = source.New("main", b.dialer("main"), logger)
	b.sub = source.New("sub", b.dialer("sub"), logger)

	id, ok := unifi.Identities[opts.Model]
	if !ok {
		id = unifi.Identities["g4doorbell"]
	}
	mac, err := NormalizeMAC(opts.MAC)
	if err != nil {
		return nil, err
	}
	cert, err := unifi.LoadCert(opts.Cert)
	if err != nil {
		return nil, err
	}
	ip := opts.IP
	if ip == "" {
		ip = LocalIP(opts.Protect.Host)
	}

	tokens := &tokenSource{token: opts.Protect.Token}
	var protect *unifi.ProtectClient
	if opts.Protect.Username != "" {
		protect = &unifi.ProtectClient{Host: opts.Protect.Host, Username: opts.Protect.Username, Password: opts.Protect.Password}
		tokens.protect = protect
	}

	b.proto = &unifi.Camera{
		Host:     opts.Protect.Host,
		Name:     opts.Name,
		MAC:      mac,
		IP:       ip,
		Doorbell: true,
		HasAudio: true,
		Identity: id,
		Cert:     cert,
		Token:    tokens.get,
		Channel:  b.channel,
		Start:    b.start,
		Stop:     func(channel string) { b.stop(channel) },
		Snapshot: b.Snapshot,
		Log:      logger,
	}
	if opts.Ring == "api" {
		if protect == nil {
			return nil, errors.New(`ring "api" needs the Protect user`)
		}
		b.proto.RingAPI = func(ctx context.Context) error { return protect.Ring(ctx, mac) }
	}
	if opts.Talkback {
		t := &talker{key: opts.Name, src: b.main, ffmpeg: opts.FFmpeg}
		b.proto.Talk = t.feed
	}
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
		if err = client.Probe(); err != nil {
			_ = client.Close()
			return nil, err
		}
		return client, nil
	}
}

// Run serves Protect until ctx ends.
func (b *Bridge) Run(ctx context.Context) {
	go b.main.Run(ctx)
	go b.sub.Run(ctx)

	// Protect caches resolution and capabilities at adoption: learn them first.
	b.probe(ctx)
	if ctx.Err() != nil {
		return
	}
	if b.opts.Events {
		go b.runEvents(ctx)
	}
	b.proto.Run(ctx)

	for _, name := range channelNames {
		if r := b.stop(name); r != nil {
			<-r.done
		}
	}
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
				log.Info().Msgf("[bridge] %s stream %dx%d %d fps audio=%t", src.Name, info.width, info.height, info.fps, info.audio)
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

var errNoVideo = errors.New("stream has no H.264 video (H.265 is not supported)")

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
	info := streamInfo{audio: audio != nil}

	frames := cons.CountFrames(3 * time.Second)
	info.fps = commonFPS(frames / 3)
	info.width, info.height = cons.Size()
	if info.width == 0 {
		sps, _ := h264.GetParameterSet(video.FmtpLine)
		if s := h264.DecodeSPS(sps); s != nil {
			info.width, info.height = int(s.Width()), int(s.Height())
		}
	}
	return info, nil
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
	info := b.info[min(channelIndex(name), 1)]
	return unifi.Channel{Width: info.width, Height: info.height, FPS: info.fps}
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

func (b *Bridge) start(channel, addr, streamName string) {
	prev := b.stop(channel)

	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan struct{})}

	b.mu.Lock()
	b.runners[channel] = r
	b.mu.Unlock()

	go func() {
		defer close(r.done)
		if prev != nil {
			<-prev.done // never two streams for one channel
		}
		b.serve(ctx, channel, addr, streamName)
	}()
}

// stop cancels a channel without waiting: it is called from the protocol loop,
// which must keep answering Protect.
func (b *Bridge) stop(channel string) *runner {
	b.mu.Lock()
	r := b.runners[channel]
	delete(b.runners, channel)
	b.mu.Unlock()
	if r != nil {
		r.cancel()
	}
	return r
}

// serve keeps one channel streaming to Protect until stopped, re-dialling a
// lost connection to the same destination.
func (b *Bridge) serve(ctx context.Context, channel, addr, streamName string) {
	for {
		err := b.streamOnce(ctx, channel, addr, streamName)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNoVideo) {
			log.Error().Err(err).Msgf("[bridge] %s", channel)
		} else {
			log.Debug().Err(err).Msgf("[bridge] %s ended, retrying", channel)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (b *Bridge) streamOnce(ctx context.Context, channel, addr, streamName string) error {
	i := channelIndex(channel)
	src := b.sourceFor(channel)

	cons := unifi.NewConsumer(unifi.StreamOptions{
		StreamName: streamName,
		Channel:    i,
		FPS:        b.channel(channel).FPS,
		Bitrate:    channelBitrates[i],
	})
	cons.RemoteAddr = addr
	cons.URL = "unifi:" + channel

	// Source first: while the doorbell is offline Protect sees no empty connections.
	addCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := src.AddConsumer(addCtx, cons)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		src.RemoveConsumer(cons)
		_ = cons.Stop()
	}()

	if video, _ := cons.TrackCodecs(); video == nil {
		return errNoVideo
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

	// Protect sends small messages back on this socket; reading them keeps the
	// TCP window open and notices Protect closing the connection.
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		_ = cons.Stop()
	}()

	_, err = cons.WriteTo(conn)
	return err
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
	b.snap, b.snapErr, b.snapTime = jpeg, err, time.Now()
	return jpeg, err
}

func (b *Bridge) captureSnapshot(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

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
	switch cons.CodecName() {
	case core.CodecJPEG:
		return mjpeg.FixJPEG(frame), nil
	}

	cmd := exec.CommandContext(ctx, b.opts.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-i", "-", "-frames:v", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "mjpeg", "-")
	cmd.Stdin = bytes.NewReader(frame)
	return cmd.Output()
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
		b.mu.Unlock()
		if i == 0 {
			st.Doorbell.Main = ss
		} else {
			st.Doorbell.Sub = ss
		}
	}
	st.Protect = b.proto.Status()
	b.mu.Lock()
	for _, name := range channelNames {
		if b.runners[name] != nil {
			st.Channels = append(st.Channels, name)
		}
	}
	st.Recent = append([]Event(nil), b.events...)
	st.Doorbell.Events = b.eventsUp
	b.mu.Unlock()
	return st
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
