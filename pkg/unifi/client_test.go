package unifi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type fakeProtect struct {
	t       *testing.T
	srv     *httptest.Server
	conns   chan *websocket.Conn
	headers chan http.Header
	uploads chan *http.Request
	bodies  chan []byte
}

func newFakeProtect(t *testing.T) *fakeProtect {
	p := &fakeProtect{
		t:       t,
		conns:   make(chan *websocket.Conn, 4),
		headers: make(chan http.Header, 4),
		uploads: make(chan *http.Request, 4),
		bodies:  make(chan []byte, 4),
	}
	up := websocket.Upgrader{
		Subprotocols: []string{"secure_transfer"},
		// real cameras send Origin http://ws_camera_proto_secure_transfer
		CheckOrigin: func(*http.Request) bool { return true },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/camera/1.0/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "good" {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		if len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no client cert", http.StatusBadRequest)
			return
		}
		p.headers <- r.Header.Clone()
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		p.conns <- conn
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("payload")
		if err == nil {
			b, _ := io.ReadAll(file)
			p.bodies <- b
		}
		p.uploads <- r
	})

	p.srv = httptest.NewUnstartedServer(mux)
	p.srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	p.srv.StartTLS()
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProtect) hostPort() (string, int) {
	u, _ := url.Parse(p.srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	n, _ := strconv.Atoi(port)
	return host, n
}

func (p *fakeProtect) accept() *websocket.Conn {
	select {
	case c := <-p.conns:
		return c
	case <-time.After(5 * time.Second):
		p.t.Fatal("camera did not connect")
		return nil
	}
}

func sendMsg(t *testing.T, conn *websocket.Conn, id int, fn string, expected bool, payload any) {
	raw, _ := json.Marshal(payload)
	b, _ := json.Marshal(Message{
		From: "UniFiVideo", FunctionName: fn, MessageID: id,
		Payload: raw, ResponseExpected: expected, To: "ubnt_avclient",
	})
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, b))
}

func readMsg(t *testing.T, conn *websocket.Conn) (Message, map[string]any) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, b, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, typ)
	var msg Message
	require.NoError(t, json.Unmarshal(b, &msg))
	var payload map[string]any
	_ = json.Unmarshal(msg.Payload, &payload)
	require.Equal(t, "ubnt_avclient", msg.From)
	require.Equal(t, "UniFiVideo", msg.To)
	return msg, payload
}

type callLog struct {
	mu      sync.Mutex
	starts  []string
	stops   []string
	tokens  []bool
	current string
}

func TestCameraSession(t *testing.T) {
	p := newFakeProtect(t)
	host, port := p.hostPort()

	pemText, err := GenerateCert()
	require.NoError(t, err)
	cert, err := LoadCert(pemText)
	require.NoError(t, err)

	calls := &callLog{}
	cam := &Camera{
		Host: host, Port: port, Name: "Doorbell", MAC: "02A1B2C3D4E5", IP: "192.0.2.36",
		Doorbell: true, HasAudio: true, Identity: G4Doorbell, Cert: cert,
		// like tokenSource: a cached token that turns out expired (403) is
		// replaced by a fresh one, which is then reused
		Token: func(_ context.Context, fresh bool) (string, error) {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			calls.tokens = append(calls.tokens, fresh)
			if fresh {
				calls.current = "good"
			}
			if calls.current == "" {
				return "expired", nil
			}
			return calls.current, nil
		},
		Channel: func(name string) Channel {
			if name == "video1" {
				return Channel{Width: 2560, Height: 1920, FPS: 20}
			}
			return Channel{Width: 640, Height: 480, FPS: 15}
		},
		Start: func(ch, addr, name, _ string) {
			calls.mu.Lock()
			calls.starts = append(calls.starts, ch+" "+addr+" "+name)
			calls.mu.Unlock()
		},
		Stop: func(ch string) {
			calls.mu.Lock()
			calls.stops = append(calls.stops, ch)
			calls.mu.Unlock()
		},
		Snapshot: func(context.Context) ([]byte, error) { return []byte("jpeg!"), nil },
		Log:      zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.DebugLevel),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cam.Run(ctx)

	conn := p.accept()
	h := <-p.headers
	require.Equal(t, "02A1B2C3D4E5", h.Get("camera-mac"))
	require.Equal(t, "0xa571", h.Get("camera-model"))
	require.Equal(t, "5.5.45", h.Get("camera-firmware"))
	require.Equal(t, "192.0.2.36", h.Get("camera-ip"))
	require.NotEmpty(t, h.Get("device-id"))
	require.Equal(t, "http://ws_camera_proto_secure_transfer", h.Get("Origin"))

	calls.mu.Lock()
	require.Equal(t, []bool{false, true}, calls.tokens)
	calls.mu.Unlock()

	// 1. hello with the full identity block
	msg, hello := readMsg(t, conn)
	require.Equal(t, "ubnt_avclient_hello", msg.FunctionName)
	require.Equal(t, "good", hello["adoptionCode"])
	require.Equal(t, float64(0xa571), hello["sysid"])
	require.Equal(t, "s5l", hello["platform"])
	require.Equal(t, "0dda191.261027.0531", hello["firmwareBuild"])
	require.Equal(t, "02:a1:b2:c3:d4:e5", hello["hwaddr"])
	require.Equal(t, true, hello["isDoorbellSeries"])
	features := hello["features"].(map[string]any)
	require.Equal(t, true, features["doorbell"])
	require.Equal(t, []any{"h264"}, features["videoCodecs"])

	// 2. time -> paramAgreement reply (as the proxy does)
	sendMsg(t, conn, 1, "ubnt_avclient_time", true, map[string]any{})
	msg, payload := readMsg(t, conn)
	require.Equal(t, "ubnt_avclient_paramAgreement", msg.FunctionName)
	require.Equal(t, 1, msg.InResponseTo)
	require.InDelta(t, float64(time.Now().UnixMilli()), payload["wallMs"], 5000)

	// 3. paramAgreement -> authToken
	sendMsg(t, conn, 2, "ubnt_avclient_paramAgreement", true, map[string]any{})
	msg, payload = readMsg(t, conn)
	require.Equal(t, "ubnt_avclient_paramAgreement", msg.FunctionName)
	require.Equal(t, "good", payload["authToken"])

	// 4. a message without responseExpected is ignored...
	sendMsg(t, conn, 3, "ChangeSomething", false, map[string]any{"a": 1})
	// ...and an unknown request is acknowledged by echoing its payload
	sendMsg(t, conn, 4, "ChangeClarityZones", true, map[string]any{"zones": []int{1, 2}})
	msg, payload = readMsg(t, conn)
	require.Equal(t, "ChangeClarityZones", msg.FunctionName)
	require.Equal(t, 4, msg.InResponseTo)
	require.Equal(t, []any{float64(1), float64(2)}, payload["zones"])

	// 5. ChangeVideoSettings starts video1, ignores video2's /dev/null
	sendMsg(t, conn, 5, "ChangeVideoSettings", true, map[string]any{
		"video": map[string]any{
			"video1": map[string]any{"avSerializer": map[string]any{
				"destinations": []string{"tcp://10.0.0.1:7550"},
				"parameters":   map[string]any{"streamName": "abcd_0"},
			}},
			"video2": map[string]any{"avSerializer": map[string]any{
				"destinations": []string{"file:///dev/null"},
			}},
			"video3": nil,
		},
	})
	msg, payload = readMsg(t, conn)
	require.Equal(t, "ChangeVideoSettings", msg.FunctionName)
	video := payload["video"].(map[string]any)
	v1 := video["video1"].(map[string]any)
	require.Equal(t, float64(2560), v1["width"])
	require.Equal(t, float64(20), v1["fps"])
	ser := v1["avSerializer"].(map[string]any)
	require.Equal(t, []any{"tcp://10.0.0.1:7550"}, ser["destinations"])
	require.Equal(t, "abcd_0", ser["parameters"].(map[string]any)["streamName"])
	require.Equal(t, float64(640), video["video2"].(map[string]any)["width"])

	calls.mu.Lock()
	require.Equal(t, []string{"video1 10.0.0.1:7550 abcd_0"}, calls.starts)
	calls.mu.Unlock()

	// the same request again does not restart the stream
	sendMsg(t, conn, 6, "ChangeVideoSettings", true, map[string]any{
		"video": map[string]any{"video1": map[string]any{"avSerializer": map[string]any{
			"destinations": []string{"tcp://10.0.0.1:7550"},
			"parameters":   map[string]any{"streamName": "abcd_0"},
		}}},
	})
	readMsg(t, conn)
	calls.mu.Lock()
	require.Len(t, calls.starts, 1)
	calls.mu.Unlock()

	// 6. GetRequest uploads a snapshot as multipart and replies
	sendMsg(t, conn, 7, "GetRequest", true, map[string]any{
		"what": "snapshot", "uri": p.srv.URL + "/upload",
		"formFields": map[string]string{"camera": "02A1B2C3D4E5"},
	})
	select {
	case r := <-p.uploads:
		require.Equal(t, "02A1B2C3D4E5", r.FormValue("camera"))
		require.Equal(t, []byte("jpeg!"), <-p.bodies)
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot upload")
	}
	msg, _ = readMsg(t, conn)
	require.Equal(t, "GetRequest", msg.FunctionName)
	require.Equal(t, 7, msg.InResponseTo)

	// 7. Reboot: the camera drops the connection, stops its streams and comes back
	sendMsg(t, conn, 8, "Reboot", false, map[string]any{})
	conn2 := p.accept()
	msg, _ = readMsg(t, conn2)
	require.Equal(t, "ubnt_avclient_hello", msg.FunctionName)

	calls.mu.Lock()
	require.Equal(t, []string{"video1"}, calls.stops)
	require.Equal(t, []bool{false, true, false}, calls.tokens) // cached token reused after reboot
	calls.mu.Unlock()
}

func TestProtectAdoptionToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "go2rtc" || body["password"] != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "session", Path: "/"})
		w.Header().Set("X-CSRF-Token", "csrf1")
	})
	mux.HandleFunc("/proxy/protect/api/cameras/manage-payload", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("TOKEN")
		if err != nil || c.Value != "session" || r.Header.Get("X-CSRF-Token") != "csrf1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"mgmt":{"token":"adopt-me","hosts":["10.0.0.1"]}}`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	p := &ProtectClient{Host: u.Host, Username: "go2rtc", Password: "secret"}
	token, err := p.AdoptionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "adopt-me", token)

	p.Password = "wrong"
	_, err = p.AdoptionToken(context.Background())
	require.Error(t, err)
}

// The console's key is trusted on first use; a different key later gets no
// password.
func TestProtectCertPin(t *testing.T) {
	logins := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins++
		w.Header().Set("X-CSRF-Token", "csrf1")
	})
	mux.HandleFunc("/proxy/protect/api/cameras/manage-payload", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"mgmt":{"token":"adopt-me"}}`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	want := CertPinOf(srv.Certificate())

	var learned string
	p := &ProtectClient{Host: u.Host, Username: "u", Password: "secret", OnPin: func(pin string) { learned = pin }}
	_, err := p.AdoptionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, learned)
	require.Equal(t, 1, logins)

	// the pinned key: fine
	p = &ProtectClient{Host: u.Host, Username: "u", Password: "secret", CertPin: want}
	_, err = p.AdoptionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, logins)

	// another key: refused before the password goes out
	p = &ProtectClient{Host: u.Host, Username: "u", Password: "secret", CertPin: strings.Repeat("ab", 32)}
	_, err = p.AdoptionToken(context.Background())
	var pinErr *PinError
	require.ErrorAs(t, err, &pinErr)
	require.Equal(t, want, pinErr.Got)
	require.Equal(t, 2, logins)
}

// A ChangeVideoSettings that names only some channels must not report the
// others as stopped while they stream.
func TestChangeVideoSettingsPartial(t *testing.T) {
	var started, stopped []string
	c := &Camera{
		Name:    "t",
		Channel: func(string) Channel { return Channel{Width: 640, Height: 480, FPS: 15} },
		Start:   func(ch, _, _, _ string) { started = append(started, ch) },
		Stop:    func(ch string) { stopped = append(stopped, ch) },
		Log:     zerolog.Nop(),
	}
	c.init()

	dests := func(res map[string]any, name string) []string {
		return res["video"].(map[string]any)[name].(map[string]any)["avSerializer"].(map[string]any)["destinations"].([]string)
	}

	res := c.changeVideoSettings(json.RawMessage(`{"video":{"video1":{"avSerializer":{
		"destinations":["tcp://10.0.0.1:7550?retryInterval=1&connectTimeout=5"],
		"parameters":{"streamName":"a"}}}}}`))
	require.Equal(t, []string{"video1"}, started)
	require.Equal(t, []string{"tcp://10.0.0.1:7550?retryInterval=1&connectTimeout=5"}, dests(res, "video1"))
	require.Equal(t, []string{"file:///dev/null"}, dests(res, "video2"))

	// only video2: video1 keeps streaming and is reported with its destination
	res = c.changeVideoSettings(json.RawMessage(`{"video":{"video2":{"avSerializer":{
		"destinations":["tcp://10.0.0.1:7551"],"parameters":{"streamName":"b"}}}}}`))
	require.Equal(t, []string{"video1", "video2"}, started)
	require.Empty(t, stopped)
	require.Equal(t, []string{"tcp://10.0.0.1:7550?retryInterval=1&connectTimeout=5"}, dests(res, "video1"))

	// an empty list stops the channel
	res = c.changeVideoSettings(json.RawMessage(`{"video":{"video1":{"avSerializer":{"destinations":[]}}}}`))
	require.Equal(t, []string{"video1"}, stopped)
	require.Empty(t, dests(res, "video1"))
	res = c.changeVideoSettings(json.RawMessage(`{}`))
	require.Equal(t, []string{"file:///dev/null"}, dests(res, "video1"))
	require.Equal(t, []string{"tcp://10.0.0.1:7551"}, dests(res, "video2"))
}

// Protect names the codec per channel ("type"); a switch restarts the stream,
// and H.265 is only offered when the main stream is H.265.
func TestChangeVideoSettingsCodec(t *testing.T) {
	type start struct{ ch, codec string }
	var started []start
	mainCodec := "h264"
	c := &Camera{
		Name: "t",
		Channel: func(name string) Channel {
			if name == "video1" {
				return Channel{Width: 2560, Height: 1920, FPS: 15, Codec: mainCodec}
			}
			return Channel{Width: 640, Height: 480, FPS: 15}
		},
		Start: func(ch, _, _, codec string) { started = append(started, start{ch, codec}) },
		Stop:  func(string) {},
		Log:   zerolog.Nop(),
	}
	c.init()
	require.Equal(t, []string{"h264"}, c.featureFlags()["videoCodecs"])
	mainCodec = "h265"
	require.Equal(t, []string{"h264", "h265"}, c.featureFlags()["videoCodecs"])

	types := func(res map[string]any) (string, string, string) {
		v := res["video"].(map[string]any)
		return v["videoCodec"].(string), v["video1"].(map[string]any)["type"].(string), v["video2"].(map[string]any)["type"].(string)
	}

	// before Protect names a codec: what the source delivers
	res := c.changeVideoSettings(json.RawMessage(`{"video":{"video1":{"avSerializer":{
		"destinations":["tcp://10.0.0.1:7550"],"parameters":{"streamName":"a"}}}}}`))
	require.Equal(t, []start{{"video1", "h264"}}, started) // Protect's default until it says otherwise
	vc, t1, t2 := types(res)
	require.Equal(t, [3]string{"h265", "h265", "h264"}, [3]string{vc, t1, t2})

	// enhanced encoding: every channel in H.265, the running stream restarts
	res = c.changeVideoSettings(json.RawMessage(`{"video":{
		"video1":{"type":"h265","avSerializer":{"destinations":["tcp://10.0.0.1:7550"],"parameters":{"streamName":"a"}}},
		"video2":{"type":"h265","avSerializer":{"destinations":["tcp://10.0.0.1:7551"],"parameters":{"streamName":"b"}}}}}`))
	require.Equal(t, []start{{"video1", "h264"}, {"video1", "h265"}, {"video2", "h265"}}, started)
	vc, t1, t2 = types(res)
	require.Equal(t, [3]string{"h265", "h265", "h265"}, [3]string{vc, t1, t2})
}

// The reply offers the doorbell's frame rates and bitrates and, with
// EchoVideo, reports a requested frame rate at once (rounded to the offer).
func TestChangeVideoSettingsEncoder(t *testing.T) {
	c := &Camera{
		Name:      "t",
		EchoVideo: true,
		Channel: func(string) Channel {
			return Channel{Width: 2560, Height: 1920, FPS: 20, FPSValues: []int{2, 10, 15, 20}, Bitrate: 6144000, BitrateMin: 4096000, BitrateMax: 8192000}
		},
		Start: func(string, string, string, string) {},
		Stop:  func(string) {},
		Log:   zerolog.Nop(),
	}
	c.init()
	video1 := func(res map[string]any) map[string]any {
		return res["video"].(map[string]any)["video1"].(map[string]any)
	}

	v := video1(c.changeVideoSettings(json.RawMessage(`{}`)))
	require.Equal(t, 20, v["fps"])
	require.Equal(t, []int{2, 10, 15, 20}, v["validFpsValues"])
	require.Equal(t, 20, v["maxFps"])
	require.Equal(t, 6144000, v["bitRateVbrMax"])
	require.Equal(t, [2]any{4096000, 8192000}, [2]any{v["validBitrateRangeMin"], v["validBitrateRangeMax"]})

	v = video1(c.changeVideoSettings(json.RawMessage(`{"video":{"video1":{"fps":12}}}`)))
	require.Equal(t, 10, v["fps"]) // 12 is not offered: the closest is 10

	c.EchoVideo = false
	c.fpsReq = map[string]int{}
	v = video1(c.changeVideoSettings(json.RawMessage(`{"video":{"video1":{"fps":15}}}`)))
	require.Equal(t, 20, v["fps"]) // not applied to the doorbell: its real rate
}

// Ring through Protect's API reuses its login and logs in again when refused.
// (Against a local fake console only.)
func TestProtectRing(t *testing.T) {
	var logins, rings int
	session := "s1"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins++
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: session, Path: "/"})
		w.Header().Set("X-CSRF-Token", "csrf")
	})
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("TOKEN")
		return err == nil && c.Value == session
	}
	mux.HandleFunc("/proxy/protect/api/cameras", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"other","mac":"AABBCCDDEEFF"},{"id":"cam1","mac":"02A1B2C3D4E5"}]`))
	})
	mux.HandleFunc("/proxy/protect/api/events/access/action", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		require.Equal(t, "door_bell", body["action"])
		require.Equal(t, "cam1", body["camera_id"])
		rings++
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	p := &ProtectClient{Host: u.Host, Username: "go2rtc", Password: "secret"}
	ctx := context.Background()
	require.NoError(t, p.Ring(ctx, "02A1B2C3D4E5"))
	require.NoError(t, p.Ring(ctx, "02A1B2C3D4E5"))
	require.Equal(t, 1, logins) // session reused
	require.Equal(t, 2, rings)

	session = "s2" // the console dropped the session: one re-login, then success
	require.NoError(t, p.Ring(ctx, "02A1B2C3D4E5"))
	require.Equal(t, 2, logins)
	require.Equal(t, 3, rings)

	require.Error(t, p.Ring(ctx, "000000000000"))
}
