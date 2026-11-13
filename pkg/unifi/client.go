package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// Camera speaks the ubnt_avclient protocol to UniFi Protect on behalf of one
// virtual camera: it connects to wss://<host>:7442, adopts with a token, answers
// Protect's requests and starts/stops the video channels Protect asks for.
// The protocol follows unifi-cam-proxy (keshavdv, woutersteegers fork), which
// is proven against Protect 7.x; deviations are commented.
type Camera struct {
	Host     string // Protect console address
	Port     int    // camera WebSocket port, default 7442
	Name     string
	MAC      string // 12 upper-case hex digits, the camera's identity in Protect
	IP       string // address Protect records for the camera
	Doorbell bool
	HasAudio bool
	Identity Identity
	Cert     tls.Certificate

	// Token returns the adoption token. It is called before every connection
	// attempt with fresh=false, and with fresh=true after Protect refused one.
	Token func(ctx context.Context, fresh bool) (string, error)

	// Channel reports what a video channel ("video1".."video3") carries.
	Channel func(name string) Channel
	// Start streams a channel to addr (host:port) as streamName, in the codec
	// Protect asks for ("h264" or "h265", the camera's videoCodec). It replaces
	// a running stream of the same channel. Stop ends it.
	Start func(channel, addr, streamName, codec string)
	Stop  func(channel string)
	// EchoVideo reports the frame rate Protect asks for right away (rounded
	// to the channel's FPSValues) because the caller applies it to the real
	// camera. Protect takes the camera's reported fps over its own, so
	// reporting the old value until the change is through would undo it.
	EchoVideo bool
	// Snapshot returns a current JPEG.
	Snapshot func(ctx context.Context) ([]byte, error)
	// RingAPI, if set, rings through Protect's HTTP API instead of the camera
	// protocol (see Ring).
	RingAPI func(ctx context.Context) error
	// Settings, if set, receives Protect's camera settings (settings.go).
	Settings Settings
	// Talk, if set, makes the camera a speaker: it receives Protect's talkback
	// audio, one ADTS AAC frame (22.05 kHz mono) per call. See talkback.go.
	Talk func(adts []byte)

	Log zerolog.Logger

	startOnce sync.Once
	quitInit  sync.Once
	quitOnce  sync.Once
	quit      chan struct{} // closed when Run returns: ends the event and settings workers
	started   time.Time     // set once, first use: uptime() runs on event goroutines too

	mu          sync.Mutex
	conn        *websocket.Conn
	msgID       int
	token       string
	firmware    string // set by a pretended firmware upgrade
	streamNames map[string]string
	streaming   map[string]string   // channel -> addr|streamName|codec currently started
	codecs      map[string]string   // channel -> codec Protect asked for
	fpsReq      map[string]int      // channel -> frame rate Protect asked for (EchoVideo)
	dests       map[string][]string // channel -> destinations Protect gave it

	httpOnce sync.Once
	http     *http.Client

	ev   events
	talk talkback
	st   Status

	hdr           bool // HDR as Protect last set it
	settingsOnce  sync.Once
	settingsQueue chan func()
}

// Status describes the connection to Protect, for a status page.
type Status struct {
	Connected  bool      `json:"connected"`
	Since      time.Time `json:"since,omitzero"`
	Controller string    `json:"controller,omitempty"` // Protect version
	Error      string    `json:"error,omitempty"`      // why the last session ended
	Talkback   bool      `json:"talkback"`             // listening for Protect's talkback audio
}

// Status returns the current connection state.
func (c *Camera) Status() Status {
	c.mu.Lock()
	st := c.st
	c.mu.Unlock()
	c.talk.mu.Lock()
	st.Talkback = c.talk.conn != nil
	c.talk.mu.Unlock()
	return st
}

const (
	pingInterval = 20 * time.Second // the Python websockets default the proxy runs with
	readTimeout  = 60 * time.Second
	maxMessage   = 4 << 20 // Protect's messages are a few KiB of JSON
)

// Run connects and serves Protect until ctx is cancelled, reconnecting with
// backoff after any error.
func (c *Camera) Run(ctx context.Context) {
	c.init()
	defer c.closeTalkback()
	defer c.quitOnce.Do(func() { close(c.stopped()) })

	backoff := 2 * time.Second
	fresh := false

	for ctx.Err() == nil {
		connected, err := c.session(ctx, fresh)
		c.stopAll()
		c.resetEvents()
		if err != nil {
			c.mu.Lock()
			c.st.Error = err.Error()
			c.mu.Unlock()
		}

		var refused *refusedError
		wasFresh := fresh
		fresh = errors.As(err, &refused) && refused.status == http.StatusForbidden

		var wait time.Duration
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errReconnect):
			c.Log.Info().Msgf("[unifi] %s: reconnecting (%v)", c.Name, err)
			wait = time.Second
		case fresh && !wasFresh:
			// the cached token expired: fetch a new one and retry at once
			c.Log.Info().Msgf("[unifi] %s: Protect refused the adoption token, fetching a new one", c.Name)
		case fresh:
			c.Log.Warn().Msgf("[unifi] %s: Protect refused a fresh adoption token", c.Name)
			wait = backoff
		default:
			c.Log.Warn().Err(err).Msgf("[unifi] %s: disconnected", c.Name)
			wait = backoff
		}

		if connected {
			backoff = 2 * time.Second
		} else if wait == backoff && backoff < 10*time.Second {
			backoff *= 2
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

type refusedError struct{ status int }

func (e *refusedError) Error() string {
	return fmt.Sprintf("handshake refused: HTTP %d", e.status)
}

// errReconnect makes the session end cleanly so Run reconnects at once (after a
// reboot request or a pretended firmware upgrade, like a real camera would).
var errReconnect = errors.New("reconnect requested by Protect")

func (c *Camera) session(ctx context.Context, freshToken bool) (connected bool, err error) {
	token, err := c.Token(ctx, freshToken)
	if err != nil {
		return false, fmt.Errorf("adoption token: %w", err)
	}

	id := c.identity()
	header := http.Header{}
	// Protect (ds since 7.3) looks the camera up by this header, as 12 hex digits.
	header.Set("camera-mac", c.MAC)
	header.Set("camera-model", fmt.Sprintf("0x%04x", id.SysID)) // the hex sysid, not the name
	header.Set("camera-ip", c.IP)
	header.Set("camera-firmware", id.Firmware)
	header.Set("device-id", uuid.NewSHA1(uuid.NameSpaceDNS, []byte("reolink-protect-bridge-"+c.MAC)).String())
	header.Set("x-guid", uuid.NewString())
	// Real cameras (firmware: "ws_camera_proto_" + subprotocol) send this origin.
	// The Adopted header they also send is left out: unifi-cam-proxy works
	// without it, and a wrong value is riskier than none.
	header.Set("Origin", "http://ws_camera_proto_secure_transfer")

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{c.Cert},
			InsecureSkipVerify: true, // Protect presents a self-signed certificate
		},
		Subprotocols:     []string{"secure_transfer"},
		HandshakeTimeout: 15 * time.Second,
	}

	u := fmt.Sprintf("wss://%s/camera/1.0/ws?token=%s", net.JoinHostPort(c.Host, strconv.Itoa(c.port())), url.QueryEscape(token))
	conn, resp, err := dialer.DialContext(ctx, u, header)
	if err != nil {
		if resp != nil {
			return false, &refusedError{status: resp.StatusCode}
		}
		return false, err
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)

	c.mu.Lock()
	c.conn = conn
	c.token = token
	c.st = Status{Connected: true, Since: time.Now()}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.st.Connected = false
		c.mu.Unlock()
	}()

	c.Log.Info().Msgf("[unifi] %s: connected to %s", c.Name, c.Host)

	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})

	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				// WriteControl may run alongside other writes (gorilla); no
				// c.mu, so a stalled message write doesn't delay the ping
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				if err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopPing: // session over
		}
	}()

	if err = c.send("ubnt_avclient_hello", 0, c.helloPayload(token)); err != nil {
		return true, err
	}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

		var msg Message
		if err = json.Unmarshal(data, &msg); err != nil {
			c.Log.Debug().Err(err).Msgf("[unifi] %s: bad message", c.Name)
			continue
		}
		if err = c.handle(ctx, &msg); err != nil {
			return true, err
		}
	}
}

// send writes one message as a binary frame (the Python proxy sends bytes).
func (c *Camera) send(name string, inResponseTo int, payload any) error {
	return c.sendMessage(nil, name, inResponseTo, payload, "")
}

// sendMessage writes a message on the current connection. With a non-nil session
// it is only sent if that connection is still the current one, so a late reply
// never reaches a newer session with a stale inResponseTo.
func (c *Camera) sendMessage(session *websocket.Conn, name string, inResponseTo int, payload any, timeStamp string) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || (session != nil && c.conn != session) {
		return errors.New("not connected")
	}

	c.msgID++
	b, err := json.Marshal(Message{
		From:         "ubnt_avclient",
		FunctionName: name,
		InResponseTo: inResponseTo,
		MessageID:    c.msgID,
		Payload:      raw,
		To:           "UniFiVideo",
		TimeStamp:    timeStamp,
	})
	if err != nil {
		return err
	}

	if e := c.Log.Trace(); e.Enabled() {
		e.Msgf("[unifi] %s: send %s", c.Name, redactToken(string(b), c.token)) // c.mu is held
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.BinaryMessage, b)
}

// alwaysHandled are processed even when Protect does not expect a reply.
var alwaysHandled = map[string]bool{
	"GetRequest": true, "ChangeVideoSettings": true, "UpdateFirmwareRequest": true,
	"Reboot": true, "ubnt_avclient_hello": true, "ChangeTalkbackSettings": true,
}

func (c *Camera) handle(ctx context.Context, msg *Message) error {
	c.init()
	fn := msg.FunctionName
	c.Log.Debug().Msgf("[unifi] %s: recv %s", c.Name, fn)
	if e := c.Log.Trace(); e.Enabled() {
		e.Msgf("[unifi] %s: recv %s %s", c.Name, fn, c.redact(string(msg.Payload)))
	}

	if !msg.ResponseExpected && !alwaysHandled[fn] {
		return nil
	}

	var res any // reply payload; nil means "no specific reply"
	reply := fn

	c.applySetting(fn, msg.Payload)

	switch fn {
	case "ubnt_avclient_hello":
		var p struct {
			ControllerVersion string `json:"controllerVersion"`
		}
		_ = json.Unmarshal(msg.Payload, &p)
		c.Log.Info().Msgf("[unifi] %s: Protect controller %s", c.Name, p.ControllerVersion)
		c.mu.Lock()
		c.st.Controller = p.ControllerVersion
		c.mu.Unlock()
		return nil

	case "ubnt_avclient_time":
		// The proxy answers time requests with a paramAgreement and the uptime in
		// SECONDS under "monotonicMs"; copied as-is because it is what Protect accepts.
		reply = "ubnt_avclient_paramAgreement"
		res = map[string]any{
			"monotonicMs": c.uptime().Seconds(),
			"wallMs":      time.Now().UnixMilli(),
			"features":    map[string]any{},
		}

	case "ubnt_avclient_paramAgreement":
		// Real cameras always run heartbeats as WebSocket pings ("forcibly enable"
		// in ubnt_avclient), which the connection does anyway.
		var p struct {
			HeartbeatsTimeoutMs int  `json:"heartbeatsTimeoutMs"`
			TimeSyncTimeoutMs   int  `json:"timeSyncTimeoutMs"`
			EnableStatusCodes   bool `json:"enableStatusCodes"`
		}
		if json.Unmarshal(msg.Payload, &p) == nil {
			c.Log.Debug().Msgf("[unifi] %s: paramAgreement heartbeatsTimeoutMs=%d timeSyncTimeoutMs=%d enableStatusCodes=%t",
				c.Name, p.HeartbeatsTimeoutMs, p.TimeSyncTimeoutMs, p.EnableStatusCodes)
		}
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()
		res = map[string]any{"authToken": token, "features": c.featureFlags()}

	case "ResetIspSettings":
		res = ispSettings(false)

	case "ChangeIspSettings":
		res = ispSettings(true)

	case "ChangeVideoSettings":
		res = c.changeVideoSettings(msg.Payload)

	case "ChangeDeviceSettings":
		var p struct {
			Timezone string `json:"timezone"`
		}
		_ = json.Unmarshal(msg.Payload, &p)
		if p.Timezone == "" {
			p.Timezone = "PST8PDT,M3.2.0,M11.1.0"
		}
		res = map[string]any{"name": c.Name, "timezone": p.Timezone}

	case "ChangeTalkbackSettings":
		if c.Talk != nil {
			res = c.changeTalkbackSettings(msg.Payload)
		}

	case "ChangeOsdSettings":
		res = c.osdSettings()

	case "NetworkStatus":
		res = c.networkStatus()

	case "ChangeSoundLedSettings":
		// syncCustomSound asks with {} and parses customSounds; we keep no
		// custom ringtone, the doorbell plays its own.
		if isEmptyPayload(msg.Payload) {
			res = map[string]any{"customSounds": []any{}}
		} else {
			res = soundLedSettings
		}

	case "AnalyticsTest", "UpdateUsernamePassword",
		"ChangeSmartDetectSettings", "ChangeSmartMotionSettings":
		res = map[string]any{}

	case "GetRequest":
		// Uploads take seconds; never hold up the other requests behind one.
		// The reply belongs to this session, not to a later one.
		c.mu.Lock()
		session := c.conn
		c.mu.Unlock()
		go c.answerSnapshot(ctx, session, msg)
		return nil

	case "UpdateFirmwareRequest":
		c.pretendUpgrade(msg.Payload)
		return errReconnect

	case "Reboot":
		return errReconnect
	}

	if res == nil {
		if !msg.ResponseExpected {
			return nil
		}
		// Protect 7.x stalls ~5 s on every unanswered request; acknowledge
		// everything else by echoing its payload, as the proxy does.
		var echo any = map[string]any{}
		if len(msg.Payload) > 0 && string(msg.Payload) != "null" {
			echo = msg.Payload
		}
		res = echo
	}

	return c.send(reply, msg.MessageID, res)
}

func (c *Camera) changeVideoSettings(payload json.RawMessage) map[string]any {
	var p struct {
		Video map[string]*struct {
			Type         string `json:"type"` // the camera's videoCodec
			FPS          int    `json:"fps"`
			AvSerializer *struct {
				Destinations []string `json:"destinations"`
				Parameters   *struct {
					StreamName string `json:"streamName"`
				} `json:"parameters"`
			} `json:"avSerializer"`
		} `json:"video"`
	}
	_ = json.Unmarshal(payload, &p)

	// Channels the request leaves out keep what they are doing: a real camera
	// reports its current state, not "stopped".
	dst := map[string][]string{}
	c.mu.Lock()
	for _, name := range []string{"video1", "video2", "video3"} {
		if d, ok := c.dests[name]; ok {
			dst[name] = d
		} else {
			dst[name] = []string{"file:///dev/null"}
		}
	}
	c.mu.Unlock()

	for _, name := range []string{"video1", "video2", "video3"} {
		v := p.Video[name]
		if v == nil {
			continue
		}
		c.mu.Lock()
		if v.Type == "h264" || v.Type == "h265" {
			c.codecs[name] = v.Type
		}
		if v.FPS > 0 && c.EchoVideo {
			c.fpsReq[name] = v.FPS
		}
		c.mu.Unlock()
		if v.AvSerializer == nil {
			continue
		}
		s := v.AvSerializer
		dst[name] = s.Destinations

		if len(s.Destinations) == 0 || strings.Contains(s.Destinations[0], "/dev/null") {
			c.stopChannel(name)
			continue
		}
		if s.Parameters == nil || s.Parameters.StreamName == "" {
			continue
		}
		u, err := url.Parse(s.Destinations[0])
		if err != nil || u.Port() == "" {
			continue
		}
		c.startChannel(name, u.Host, s.Parameters.StreamName, s.Destinations)
	}

	return c.videoSettings(dst)
}

func (c *Camera) startChannel(name, addr, streamName string, dests []string) {
	c.mu.Lock()
	c.dests[name] = dests
	c.streamNames[name] = streamName
	codec := c.codecs[name]
	if codec == "" {
		codec = "h264"
	}
	key := addr + "|" + streamName + "|" + codec // a codec switch restarts the stream
	same := c.streaming[name] == key
	c.streaming[name] = key
	c.mu.Unlock()

	if !same {
		c.Log.Info().Msgf("[unifi] %s: start %s -> %s (%s, %s)", c.Name, name, addr, streamName, codec)
		c.Start(name, addr, streamName, codec)
	}
}

func (c *Camera) stopChannel(name string) {
	c.mu.Lock()
	_, running := c.streaming[name]
	delete(c.streaming, name)
	delete(c.dests, name)
	c.mu.Unlock()

	if running {
		c.Log.Info().Msgf("[unifi] %s: stop %s", c.Name, name)
		c.Stop(name)
	}
}

// SetHDR tells the camera the HDR state the image sensor has, before Run:
// Protect asks for it after connecting and only switches (and waits for a
// stream restart) when it differs from what it wants.
func (c *Camera) SetHDR(on bool) {
	c.mu.Lock()
	c.hdr = on
	c.mu.Unlock()
}

// restartStreams ends and reopens every running stream, as a UniFi camera's
// encoder restart does (HDR). Protect keeps the camera's video settings
// greyed out ("reconfiguration in progress") until its high-quality stream
// reconnects (inStreamCreated).
func (c *Camera) restartStreams() {
	c.mu.Lock()
	running := make(map[string]string, len(c.streaming))
	for name, key := range c.streaming {
		running[name] = key
	}
	c.mu.Unlock()
	for _, name := range []string{"video1", "video2", "video3"} {
		key, ok := running[name]
		if !ok {
			continue
		}
		parts := strings.SplitN(key, "|", 3) // addr|streamName|codec
		if len(parts) != 3 {
			continue
		}
		c.Log.Info().Msgf("[unifi] %s: restart %s", c.Name, name)
		c.Stop(name)
		c.mu.Lock()
		still := c.streaming[name] == key // not stopped or changed meanwhile
		c.mu.Unlock()
		if still {
			c.Start(name, parts[0], parts[1], parts[2])
		}
	}
}

func (c *Camera) stopAll() {
	for _, name := range []string{"video1", "video2", "video3"} {
		c.stopChannel(name)
	}
}

func (c *Camera) pretendUpgrade(payload json.RawMessage) {
	var p struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(payload, &p)
	if v := firmwareVersion(p.URI); v != "" {
		c.Log.Info().Msgf("[unifi] %s: pretending firmware upgrade to %s", c.Name, v)
		c.mu.Lock()
		c.firmware = v
		c.mu.Unlock()
	} else {
		c.Log.Warn().Msgf("[unifi] %s: ignoring firmware push without a version: %s", c.Name, p.URI)
	}
}

// answerSnapshot uploads a JPEG to the URI Protect gives in a GetRequest, as a
// multipart form with the image in field "payload" plus Protect's formFields.
func (c *Camera) answerSnapshot(ctx context.Context, session *websocket.Conn, msg *Message) {
	var p struct {
		What       string            `json:"what"`
		URI        string            `json:"uri"`
		Filename   string            `json:"filename"`
		FormFields map[string]string `json:"formFields"`
	}
	_ = json.Unmarshal(msg.Payload, &p)

	if p.URI != "" {
		if err := c.uploadSnapshot(ctx, p.URI, p.FormFields, c.eventFile(p.What, p.Filename)); err != nil {
			c.Log.Warn().Err(err).Msgf("[unifi] %s: snapshot %s", c.Name, p.What)
		} else {
			c.Log.Debug().Msgf("[unifi] %s: uploaded snapshot %s", c.Name, p.What)
		}
	}

	if msg.ResponseExpected && session != nil {
		_ = c.sendMessage(session, "GetRequest", msg.MessageID, nil, "")
	}
}

// uploadSnapshot uploads jpeg, or a current snapshot when it is nil (an event
// file Protect asks for by name is passed in).
func (c *Camera) uploadSnapshot(ctx context.Context, uri string, fields map[string]string, jpeg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if jpeg == nil {
		var err error
		if jpeg, err = c.Snapshot(ctx); err != nil {
			return err
		}
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("payload", "screen.jpg")
	if err != nil {
		return err
	}
	if _, err = part.Write(jpeg); err != nil {
		return err
	}
	for k, v := range fields {
		if err = w.WriteField(k, v); err != nil {
			return err
		}
	}
	if err = w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.uploadClient().Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // keeps the connection reusable
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("upload: HTTP %d", resp.StatusCode)
	}
	return nil
}

// uploadClient is one shared HTTPS client for snapshot uploads, presenting the
// camera certificate.
func (c *Camera) uploadClient() *http.Client {
	c.httpOnce.Do(func() {
		c.http = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{c.Cert},
				InsecureSkipVerify: true,
			},
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     30 * time.Second,
		}}
	})
	return c.http
}

// redact hides the adoption/auth token in trace logs. Not with c.mu held.
func (c *Camera) redact(s string) string {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	return redactToken(s, token)
}

func redactToken(s, token string) string {
	if len(token) < 4 {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

func (c *Camera) port() int {
	if c.Port != 0 {
		return c.Port
	}
	return 7442
}

func (c *Camera) init() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uptime() // the clock starts with the first Run or message
	if c.streamNames == nil {
		c.streamNames = map[string]string{}
		c.streaming = map[string]string{}
		c.codecs = map[string]string{}
		c.fpsReq = map[string]int{}
		c.dests = map[string][]string{}
	}
}

func (c *Camera) identity() Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.Identity
	if c.firmware != "" {
		id.Firmware = c.firmware
	}
	return id
}

func (c *Camera) channel(name string) Channel {
	ch := Channel{Width: 1920, Height: 1080, FPS: 15, Codec: "h264"}
	if c.Channel != nil {
		got := c.Channel(name)
		if got.Codec != "" {
			ch.Codec = got.Codec
		}
		if got.Width > 0 && got.Height > 0 {
			ch.Width, ch.Height = got.Width, got.Height
		}
		if got.FPS > 0 {
			ch.FPS = got.FPS
		}
		ch.FPSValues, ch.Bitrate = got.FPSValues, got.Bitrate
		if got.BitrateMax > 0 && got.BitrateMin <= got.BitrateMax {
			ch.BitrateMin, ch.BitrateMax = got.BitrateMin, got.BitrateMax
		}
	}
	return ch
}

// stopped is closed once Run has returned.
func (c *Camera) stopped() chan struct{} {
	c.quitInit.Do(func() { c.quit = make(chan struct{}) })
	return c.quit
}

// worker runs queued jobs in order until Run returns; jobs still queued then
// are dropped, so nothing reaches the doorbell or Protect after a restart.
func (c *Camera) worker(q chan func()) {
	quit := c.stopped()
	for {
		select {
		case <-quit:
			return
		case job := <-q:
			select {
			case <-quit:
				return
			default:
			}
			job()
		}
	}
}

func (c *Camera) uptime() time.Duration {
	c.startOnce.Do(func() { c.started = time.Now() })
	return time.Since(c.started)
}

func isEmptyPayload(p json.RawMessage) bool {
	s := strings.TrimSpace(string(p))
	return s == "" || s == "{}" || s == "null"
}
