package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

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
	store, err := config.Open(t.TempDir())
	require.NoError(t, err)
	s := New(Options{Store: store, Logs: logbuf.New(nil, 10), Log: zerolog.Nop(), Version: "test"})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &client{t: t, srv: srv, c: &http.Client{Jar: jar}}, store
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
		"doorbell": map[string]any{"host": "192.168.1.20", "username": "admin", "password": "door-pw"},
		"protect":  map[string]any{"host": "192.168.1.1", "token": "adopt-token"},
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
		"doorbell": map[string]any{"host": "192.168.1.21", "username": "admin", "password": ""},
		"protect":  map[string]any{"host": "192.168.1.1", "username": "local", "password": "p", "clearToken": true},
	}, true)
	require.Equal(t, 200, res.StatusCode)
	cfg := store.Get()
	require.Equal(t, "door-pw", cfg.Doorbell.Password)
	require.Equal(t, "192.168.1.21", cfg.Doorbell.Host)
	require.Empty(t, cfg.Protect.Token)

	res, _ = c.do("PUT", "/api/config", map[string]any{"camera": map[string]any{"name": "Front", "model": "nope", "ring": "camera"}}, true)
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
	c, _ := newTest(t)
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
