package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/internal/config"
	"github.com/jgerdes5/reolink-protect-bridge/internal/logbuf"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
}

func newTest(t *testing.T) (*client, *config.Store) {
	c, store, _ := newTestServer(t)
	return c, store
}

func newTestServer(t *testing.T) (*client, *config.Store, *Server) {
	store, err := config.Open(t.TempDir())
	require.NoError(t, err)
	s := New(Options{Store: store, Logs: logbuf.New(nil, 10), Log: zerolog.Nop(), Version: "test"})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &client{t: t, srv: srv, c: &http.Client{Jar: jar}}, store, s
}

func (c *client) do(method, path string, body any, csrf bool) (*http.Response, map[string]any) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	if csrf {
		req.Header.Set("X-Requested-With", "test")
	}
	res, err := c.c.Do(req)
	require.NoError(c.t, err)
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestSetupLoginFlow(t *testing.T) {
	c, store := newTest(t)

	res, _ := c.do("GET", "/healthz", nil, false)
	require.Equal(t, 200, res.StatusCode)

	_, s := c.do("GET", "/api/session", nil, false)
	require.Equal(t, true, s["setup"])

	res, _ = c.do("GET", "/api/config", nil, false)
	require.Equal(t, 401, res.StatusCode)

	// CSRF header required
	res, _ = c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, false)
	require.Equal(t, 403, res.StatusCode)

	res, _ = c.do("POST", "/api/setup", map[string]string{"password": "short"}, true)
	require.Equal(t, 400, res.StatusCode)

	res, _ = c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)
	require.Equal(t, 200, res.StatusCode)
	require.True(t, store.Get().HasAdmin())

	res, _ = c.do("POST", "/api/setup", map[string]string{"password": "other1234"}, true)
	require.Equal(t, 409, res.StatusCode)

	// logged in by setup
	res, cfg := c.do("GET", "/api/config", nil, false)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "Doorbell", cfg["camera"].(map[string]any)["name"])

	res, _ = c.do("POST", "/api/logout", nil, true)
	require.Equal(t, 200, res.StatusCode)
	res, _ = c.do("GET", "/api/status", nil, false)
	require.Equal(t, 401, res.StatusCode)

	res, _ = c.do("POST", "/api/login", map[string]string{"password": "wrong pass"}, true)
	require.Equal(t, 401, res.StatusCode)
	res, _ = c.do("POST", "/api/login", map[string]string{"password": "secret123"}, true)
	require.Equal(t, 200, res.StatusCode)

	res, st := c.do("GET", "/api/status", nil, false)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, false, st["configured"])
	require.NotEmpty(t, st["missing"])
}

func TestConfigSecretsWriteOnly(t *testing.T) {
	c, store := newTest(t)
	c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)

	res, out := c.do("PUT", "/api/config", map[string]any{
		"doorbell": map[string]any{"host": "192.0.2.20", "username": "admin", "password": "door-pw"},
		"protect":  map[string]any{"host": "192.0.2.1", "token": "adopt-token"},
	}, true)
	require.Equal(t, 200, res.StatusCode, out)
	require.True(t, store.Get().Complete())

	res, raw := c.raw("GET", "/api/config")
	require.Equal(t, 200, res.StatusCode)
	require.NotContains(t, raw, "door-pw")
	require.NotContains(t, raw, "adopt-token")
	require.Contains(t, raw, `"password":{"set":true}`)

	// empty password keeps the stored one; clearToken removes the token
	res, _ = c.do("PUT", "/api/config", map[string]any{
		"doorbell": map[string]any{"host": "192.0.2.20", "username": "admin", "password": "", "port": 9001},
		"protect":  map[string]any{"host": "192.0.2.1", "username": "local", "password": "p", "clearToken": true},
	}, true)
	require.Equal(t, 200, res.StatusCode)
	cfg := store.Get()
	require.Equal(t, "door-pw", cfg.Doorbell.Password)
	require.Equal(t, 9001, cfg.Doorbell.Port)
	require.Empty(t, cfg.Protect.Token)

	res, _ = c.do("PUT", "/api/config", map[string]any{"camera": map[string]any{"name": "Front", "ring": "nope"}}, true)
	require.Equal(t, 400, res.StatusCode)

	mac := store.Get().Identity.MAC
	res, _ = c.do("POST", "/api/identity", nil, true)
	require.Equal(t, 200, res.StatusCode)
	require.NotEqual(t, mac, store.Get().Identity.MAC)

	res, _ = c.do("GET", "/api/snapshot.jpg", nil, false)
	require.Equal(t, 503, res.StatusCode)
}

func (c *client) raw(method, path string) (*http.Response, string) {
	req, _ := http.NewRequest(method, c.srv.URL+path, nil)
	res, err := c.c.Do(req)
	require.NoError(c.t, err)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestLoginRateLimit(t *testing.T) {
	c, _, s := newTestServer(t)
	// a stopped clock: bcrypt on a slow machine must not outlast the lockout
	now := time.Now()
	s.limiter.now = func() time.Time { return now }
	c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)
	c.do("POST", "/api/logout", nil, true)
	for range 5 {
		res, _ := c.do("POST", "/api/login", map[string]string{"password": "wrong pass"}, true)
		require.Equal(t, 401, res.StatusCode)
	}
	res, _ := c.do("POST", "/api/login", map[string]string{"password": "secret123"}, true)
	require.Equal(t, 429, res.StatusCode)
}

func TestStaticAndHeaders(t *testing.T) {
	c, _ := newTest(t)
	res, body := c.raw("GET", "/")
	require.Equal(t, 200, res.StatusCode)
	require.Contains(t, body, "Reolink Protect Bridge")
	require.Contains(t, res.Header.Get("Content-Security-Policy"), "default-src 'self'")
	res, _ = c.raw("GET", "/app.js")
	require.Equal(t, 200, res.StatusCode)
}

// As a Home Assistant add-on behind Ingress: no own login, setup refused, the
// page may be framed by Home Assistant (same origin), API paths are relative.
func TestIngressMode(t *testing.T) {
	store, err := config.Open(t.TempDir())
	require.NoError(t, err)
	s := New(Options{Store: store, Logs: logbuf.New(nil, 10), Log: zerolog.Nop(), Version: "test", Ingress: true, IngressPeer: "127.0.0.1"})
	srv := httptest.NewServer(s)
	defer srv.Close()
	c := &client{t: t, srv: srv, c: &http.Client{}}

	resp, body := c.do("GET", "/api/session", nil, false)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, true, body["authenticated"])
	require.Equal(t, false, body["setup"])
	require.Equal(t, true, body["ingress"])
	require.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'self'")
	require.Empty(t, resp.Header.Get("X-Frame-Options"))

	resp, _ = c.do("GET", "/api/config", nil, false) // no session needed
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = c.do("POST", "/api/setup", map[string]string{"password": "longenough"}, true)
	require.Equal(t, http.StatusConflict, resp.StatusCode)

	resp, js := c.raw("GET", "/app.js")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, js, `"/api/`) // relative: works under /api/hassio_ingress/<token>/
}

func TestPasswordChangeAndPersistentSessions(t *testing.T) {
	c, store := newTest(t)
	res, _ := c.do("POST", "/api/setup", map[string]string{"password": "first-pass"}, true)
	require.Equal(t, 200, res.StatusCode)

	// a second browser
	jar, _ := cookiejar.New(nil)
	other := &client{t: t, srv: c.srv, c: &http.Client{Jar: jar}}
	res, _ = other.do("POST", "/api/login", map[string]string{"password": "first-pass"}, true)
	require.Equal(t, 200, res.StatusCode)

	// the session file holds hashes, not the cookie values
	data, err := os.ReadFile(filepath.Join(filepath.Dir(store.Path()), "sessions.json"))
	require.NoError(t, err)
	for _, ck := range jar.Cookies(mustURL(t, c.srv.URL)) {
		require.NotContains(t, string(data), ck.Value)
	}

	// a restarted server (same data dir) still knows the session
	again := httptest.NewServer(New(Options{Store: store, Logs: logbuf.New(nil, 10), Log: zerolog.Nop(), Version: "test"}))
	defer again.Close()
	restarted := &client{t: t, srv: again, c: &http.Client{Jar: jar}}
	for _, ck := range jar.Cookies(mustURL(t, c.srv.URL)) {
		jar.SetCookies(mustURL(t, again.URL), []*http.Cookie{ck})
	}
	res, _ = restarted.do("GET", "/api/status", nil, false)
	require.Equal(t, 200, res.StatusCode)

	res, _ = c.do("POST", "/api/password", map[string]string{"current": "wrong-pass", "password": "second-pass"}, true)
	require.Equal(t, 403, res.StatusCode)
	res, _ = c.do("POST", "/api/password", map[string]string{"current": "first-pass", "password": "short"}, true)
	require.Equal(t, 400, res.StatusCode)
	res, _ = c.do("POST", "/api/password", map[string]string{"current": "first-pass", "password": "second-pass"}, true)
	require.Equal(t, 200, res.StatusCode)

	res, _ = c.do("GET", "/api/status", nil, false) // this browser got a new session
	require.Equal(t, 200, res.StatusCode)
	res, _ = other.do("GET", "/api/status", nil, false) // the other one was logged out
	require.Equal(t, 401, res.StatusCode)
	require.True(t, store.Get().CheckAdminPassword("second-pass"))
}

func TestAdvancedSettings(t *testing.T) {
	c, store := newTest(t)
	res, _ := c.do("POST", "/api/setup", map[string]string{"password": "first-pass"}, true)
	require.Equal(t, 200, res.StatusCode)

	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	require.NoError(t, os.WriteFile(ffmpeg, []byte("#!/bin/sh\n"), 0o755))
	res, body := c.do("PUT", "/api/config", map[string]any{"advanced": map[string]string{"listen": ":9090", "ffmpeg": ffmpeg}}, true)
	require.Equal(t, 200, res.StatusCode, body)
	require.Equal(t, ":9090", body["advanced"].(map[string]any)["listen"])
	require.Equal(t, ffmpeg, store.Get().FFmpeg)

	// the ffmpeg path must not become a way to run any program
	for _, bad := range []string{"/bin/sh", "sh", filepath.Join(t.TempDir(), "ffmpeg")} {
		res, body = c.do("PUT", "/api/config", map[string]any{"advanced": map[string]string{"listen": ":9090", "ffmpeg": bad}}, true)
		require.Equal(t, 400, res.StatusCode, bad)
		require.NotEmpty(t, body["error"])
	}
	require.Equal(t, ffmpeg, store.Get().FFmpeg)

	res, _ = c.do("PUT", "/api/config", map[string]any{"advanced": map[string]string{"listen": "nonsense"}}, true)
	require.Equal(t, 400, res.StatusCode)

	res, _ = c.do("PUT", "/api/config", map[string]any{"doorbell": map[string]any{"host": "192.0.2.5", "username": "admin", "channel": 2}}, true)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, 2, store.Get().Doorbell.Channel)
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

// Behind Ingress there is no own login, so only Home Assistant's Supervisor
// may reach the page; anything else on the hassio network or the host is
// refused.
func TestIngressOnlyFromSupervisor(t *testing.T) {
	store, err := config.Open(t.TempDir())
	require.NoError(t, err)
	srv := httptest.NewServer(New(Options{Store: store, Log: zerolog.Nop(), Ingress: true})) // default peer 172.30.32.2
	defer srv.Close()
	c := &client{t: t, srv: srv, c: &http.Client{}}
	for _, path := range []string{"/", "/api/session", "/api/config", "/healthz"} {
		res, _ := c.do("GET", path, nil, false)
		require.Equal(t, http.StatusForbidden, res.StatusCode, path)
	}
	res, _ := c.do("PUT", "/api/config", map[string]any{"camera": map[string]any{"name": "x", "ring": "camera"}}, true)
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Equal(t, "Doorbell", store.Get().Camera.Name)

	// the handler sees the Supervisor's address: allowed
	h := New(Options{Store: store, Log: zerolog.Nop(), Ingress: true})
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.RemoteAddr = DefaultIngressPeer + ":41234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	req.RemoteAddr = "172.30.32.5:41234"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// DNS rebinding: the first-start setup only works when the page was opened
// by IP address or a local name.
func TestSetupHostCheck(t *testing.T) {
	c, store := newTest(t)
	post := func(host string) int {
		req, _ := http.NewRequest("POST", c.srv.URL+"/api/setup", bytes.NewReader([]byte(`{"password":"secret123"}`)))
		req.Header.Set("X-Requested-With", "test")
		req.Host = host
		res, err := c.c.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, host := range []string{"attacker.example.com", "attacker.example.com:8080", "rebind.evil.net."} {
		require.Equal(t, http.StatusForbidden, post(host), host)
	}
	require.False(t, store.Get().HasAdmin())

	s := &Server{opts: Options{Store: store}}
	for _, host := range []string{"192.0.2.10:8080", "[2001:db8::1]:8080", "localhost:8080", "bridge:8080", "bridge.local", "bridge.lan:8080", "bridge.home.arpa", "bridge.internal"} {
		require.True(t, s.setupHostAllowed(host), host)
	}
	_, err := store.Update(func(c *config.Config) error { c.Web.Listen = "bridge.example.org:8080"; return nil })
	require.NoError(t, err)
	require.True(t, s.setupHostAllowed("bridge.example.org:8080"))
	require.False(t, s.setupHostAllowed("other.example.org:8080"))

	require.Equal(t, http.StatusOK, post("192.0.2.10:8080"))
	require.True(t, store.Get().HasAdmin())
}

// A stored password or token is only sent to the address it was entered for:
// changing the address requires entering it again.
func TestHostChangeNeedsSecret(t *testing.T) {
	c, store := newTest(t)
	c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)
	res, out := c.do("PUT", "/api/config", map[string]any{
		"doorbell": map[string]any{"host": "192.0.2.20", "username": "admin", "password": "door-pw"},
		"protect":  map[string]any{"host": "192.0.2.1", "username": "local", "password": "prot-pw", "token": "adopt-token"},
	}, true)
	require.Equal(t, 200, res.StatusCode, out)

	res, out = c.do("PUT", "/api/config", map[string]any{
		"doorbell": map[string]any{"host": "198.51.100.66", "username": "admin"},
	}, true)
	require.Equal(t, 400, res.StatusCode)
	require.Contains(t, out["error"], "doorbell password")
	require.Equal(t, "192.0.2.20", store.Get().Doorbell.Host)

	res, out = c.do("PUT", "/api/config", map[string]any{
		"protect": map[string]any{"host": "198.51.100.66", "username": "local", "token": "new-token"},
	}, true)
	require.Equal(t, 400, res.StatusCode)
	require.Contains(t, out["error"], "Protect password")

	res, out = c.do("PUT", "/api/config", map[string]any{
		"protect": map[string]any{"host": "198.51.100.66", "username": "local", "password": "new-pw"},
	}, true)
	require.Equal(t, 400, res.StatusCode)
	require.Contains(t, out["error"], "adoption token")
	require.Equal(t, "192.0.2.1", store.Get().Protect.Host)

	// entering the secrets again (or removing the token) works
	res, out = c.do("PUT", "/api/config", map[string]any{
		"doorbell": map[string]any{"host": "192.0.2.30", "username": "admin", "password": "door-pw2"},
		"protect":  map[string]any{"host": "192.0.2.2", "username": "local", "password": "new-pw", "clearToken": true},
	}, true)
	require.Equal(t, 200, res.StatusCode, out)
	cfg := store.Get()
	require.Equal(t, "door-pw2", cfg.Doorbell.Password)
	require.Equal(t, "new-pw", cfg.Protect.Password)
	require.Empty(t, cfg.Protect.Token)
}

// The console's certificate key is shown, kept while the address stays, and
// forgotten when the address changes or the user asks.
func TestCertPin(t *testing.T) {
	c, store := newTest(t)
	c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)
	pin := strings.Repeat("ab", 32)
	protect := map[string]any{"host": "192.0.2.1", "username": "local", "password": "prot-pw"}
	res, out := c.do("PUT", "/api/config", map[string]any{"protect": protect}, true)
	require.Equal(t, 200, res.StatusCode, out)
	_, err := store.Update(func(cfg *config.Config) error { cfg.Protect.CertPin = pin; return nil })
	require.NoError(t, err)

	_, out = c.do("GET", "/api/config", nil, true)
	require.Equal(t, pin, out["protect"].(map[string]any)["certPin"])

	res, _ = c.do("PUT", "/api/config", map[string]any{"protect": map[string]any{"host": "192.0.2.1", "username": "local"}}, true)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, pin, store.Get().Protect.CertPin)

	res, _ = c.do("PUT", "/api/config", map[string]any{"protect": map[string]any{"host": "192.0.2.1", "username": "local", "clearCertPin": true}}, true)
	require.Equal(t, 200, res.StatusCode)
	require.Empty(t, store.Get().Protect.CertPin)

	_, err = store.Update(func(cfg *config.Config) error { cfg.Protect.CertPin = pin; return nil })
	require.NoError(t, err)
	res, _ = c.do("PUT", "/api/config", map[string]any{"protect": map[string]any{"host": "192.0.2.2", "username": "local", "password": "prot-pw"}}, true)
	require.Equal(t, 200, res.StatusCode)
	require.Empty(t, store.Get().Protect.CertPin)
}

// Parallel wrong passwords must not get more than 5 tries past the limiter.
func TestLoginRateLimitParallel(t *testing.T) {
	c, _, s := newTestServer(t)
	now := time.Now() // stopped clock: the lockout must not end mid-test
	s.limiter.now = func() time.Time { return now }
	c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)
	c.do("POST", "/api/logout", nil, true)
	var wg sync.WaitGroup
	var tried, blocked atomic.Int32
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", c.srv.URL+"/api/login", bytes.NewReader([]byte(`{"password":"wrong pass"}`)))
			req.Header.Set("X-Requested-With", "test")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_ = res.Body.Close()
			switch res.StatusCode {
			case http.StatusUnauthorized:
				tried.Add(1)
			case http.StatusTooManyRequests:
				blocked.Add(1)
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, tried.Load(), int32(5))
	require.Equal(t, int32(30), tried.Load()+blocked.Load())
}

func TestLogDownload(t *testing.T) {
	c, _ := newTest(t)
	res, _ := c.raw("GET", "/api/logs.txt")
	require.Equal(t, 401, res.StatusCode)
	c.do("POST", "/api/setup", map[string]string{"password": "secret123"}, true)
	res, _ = c.raw("GET", "/api/logs.txt")
	require.Equal(t, 200, res.StatusCode)
	require.Contains(t, res.Header.Get("Content-Disposition"), "attachment")
	require.Contains(t, res.Header.Get("Content-Type"), "text/plain")
}
