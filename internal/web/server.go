// Package web is the bridge's settings and status UI: a small JSON API and an
// embedded single-page app.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/internal/bridge"
	"github.com/jgerdes5/reolink-protect-bridge/internal/config"
	"github.com/jgerdes5/reolink-protect-bridge/internal/discovery"
	"github.com/jgerdes5/reolink-protect-bridge/internal/logbuf"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/rs/zerolog"
)

//go:embed static
var staticFS embed.FS

// Options configure the web server.
type Options struct {
	Store   *config.Store
	Runner  *Runner
	Logs    *logbuf.Buffer
	Log     zerolog.Logger
	Version string
}

// Server is the HTTP handler.
type Server struct {
	opts     Options
	sessions *sessions
	limiter  *limiter
	discover sync.Mutex
	mux      *http.ServeMux
}

// New builds the handler.
func New(opts Options) *Server {
	s := &Server{opts: opts, sessions: newSessions(), limiter: newLimiter(), mux: http.NewServeMux()}

	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	s.mux.Handle("GET /", files)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	s.mux.HandleFunc("GET /api/session", s.handleSession)
	s.mux.HandleFunc("POST /api/setup", s.handleSetup)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)

	s.mux.HandleFunc("GET /api/config", s.auth(s.handleGetConfig))
	s.mux.HandleFunc("PUT /api/config", s.auth(s.handlePutConfig))
	s.mux.HandleFunc("POST /api/identity", s.auth(s.handleNewIdentity))
	s.mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	s.mux.HandleFunc("GET /api/logs", s.auth(s.handleLogs))
	s.mux.HandleFunc("GET /api/discover", s.auth(s.handleDiscover))
	s.mux.HandleFunc("GET /api/snapshot.jpg", s.auth(s.handleSnapshot))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
		// CSRF: a cross-site form cannot set custom headers, and a
		// cross-origin fetch with one needs a CORS preflight we never allow.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") == "" {
			writeError(w, http.StatusForbidden, "missing X-Requested-With header")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.valid(sessionToken(r)) {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// --- session ---

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Store.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"setup":         !cfg.HasAdmin(),
		"authenticated": cfg.HasAdmin() && s.sessions.valid(sessionToken(r)),
		"version":       s.opts.Version,
	})
}

type passwordInput struct {
	Password string `json:"password"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var in passwordInput
	if !readJSON(w, r, &in) {
		return
	}
	_, err := s.opts.Store.Update(func(c *config.Config) error {
		if c.HasAdmin() {
			return errAdminExists
		}
		return c.SetAdminPassword(in.Password)
	})
	if errors.Is(err, errAdminExists) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.opts.Log.Info().Msg("[web] admin password created")
	s.startSession(w, r)
}

var errAdminExists = errors.New("the admin password is already set")

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if d := s.limiter.blocked(ip); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed logins, try again in "+d.Round(time.Second).String())
		return
	}
	var in passwordInput
	if !readJSON(w, r, &in) {
		return
	}
	cfg := s.opts.Store.Get()
	if !cfg.HasAdmin() {
		writeError(w, http.StatusConflict, "create the admin password first")
		return
	}
	if !cfg.CheckAdminPassword(in.Password) {
		s.limiter.fail(ip)
		s.opts.Log.Warn().Str("client", ip).Msg("[web] failed login")
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.limiter.success(ip)
	s.startSession(w, r)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	token, err := s.sessions.create()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.remove(sessionToken(r))
	clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- config ---

type secret struct {
	Set bool `json:"set"`
}

type configView struct {
	Doorbell struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password secret `json:"password"`
		Channel  int    `json:"channel"`
	} `json:"doorbell"`
	Protect struct {
		Host     string `json:"host"`
		Username string `json:"username"`
		Password secret `json:"password"`
		Token    secret `json:"token"`
	} `json:"protect"`
	Camera   config.Camera `json:"camera"`
	Identity struct {
		MAC string `json:"mac"`
	} `json:"identity"`
	Models []model `json:"models"`
}

type model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func viewOf(c config.Config) configView {
	var v configView
	v.Doorbell.Host, v.Doorbell.Port, v.Doorbell.Username, v.Doorbell.Channel = c.Doorbell.Host, c.Doorbell.Port, c.Doorbell.Username, c.Doorbell.Channel
	v.Doorbell.Password.Set = c.Doorbell.Password != ""
	v.Protect.Host, v.Protect.Username = c.Protect.Host, c.Protect.Username
	v.Protect.Password.Set = c.Protect.Password != ""
	v.Protect.Token.Set = c.Protect.Token != ""
	v.Camera = c.Camera
	v.Identity.MAC = c.Identity.MAC
	for _, id := range []string{"g4doorbell", "g5pro"} {
		if ident, ok := unifi.Identities[id]; ok {
			v.Models = append(v.Models, model{ID: id, Name: strings.TrimPrefix(ident.Model, "UVC ")})
		}
	}
	return v
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, viewOf(s.opts.Store.Get()))
}

// configInput is a PUT body. Absent sections stay unchanged; empty secrets
// keep the stored value unless the matching clear flag is set.
type configInput struct {
	Doorbell *struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		Channel  int    `json:"channel"`
	} `json:"doorbell"`
	Protect *struct {
		Host          string `json:"host"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		Token         string `json:"token"`
		ClearPassword bool   `json:"clearPassword"`
		ClearToken    bool   `json:"clearToken"`
	} `json:"protect"`
	Camera *config.Camera `json:"camera"`
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var in configInput
	if !readJSON(w, r, &in) {
		return
	}
	before := s.opts.Store.Get()
	cfg, err := s.opts.Store.Update(func(c *config.Config) error {
		if d := in.Doorbell; d != nil {
			c.Doorbell.Host = strings.TrimSpace(d.Host)
			c.Doorbell.Port = d.Port
			c.Doorbell.Username = strings.TrimSpace(d.Username)
			c.Doorbell.Channel = d.Channel
			if d.Password != "" {
				c.Doorbell.Password = d.Password
			}
		}
		if p := in.Protect; p != nil {
			c.Protect.Host = strings.TrimSpace(p.Host)
			c.Protect.Username = strings.TrimSpace(p.Username)
			switch {
			case p.Password != "":
				c.Protect.Password = p.Password
			case p.ClearPassword || c.Protect.Username == "":
				c.Protect.Password = ""
			}
			switch {
			case p.Token != "":
				c.Protect.Token = strings.TrimSpace(p.Token)
			case p.ClearToken:
				c.Protect.Token = ""
			}
		}
		if in.Camera != nil {
			cam := *in.Camera
			cam.Name = strings.TrimSpace(cam.Name)
			c.Camera = cam
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.applied(before, cfg)
	writeJSON(w, http.StatusOK, viewOf(cfg))
}

func (s *Server) handleNewIdentity(w http.ResponseWriter, _ *http.Request) {
	id, err := config.NewIdentity()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	before := s.opts.Store.Get()
	cfg, err := s.opts.Store.Update(func(c *config.Config) error {
		c.Identity = id
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.opts.Log.Warn().Str("mac", cfg.Identity.MAC).Msg("[web] new camera identity: Protect sees a new camera, adopt it again")
	s.applied(before, cfg)
	writeJSON(w, http.StatusOK, viewOf(cfg))
}

// applied updates log masking and restarts the bridge if its options changed
// (or it is not running).
func (s *Server) applied(before, after config.Config) {
	if s.opts.Logs != nil {
		s.opts.Logs.SetSecrets(after.Secrets())
	}
	if s.opts.Runner == nil {
		return
	}
	b, _ := s.opts.Runner.Current()
	if b != nil && reflect.DeepEqual(before.Options(), after.Options()) {
		return
	}
	s.opts.Runner.Restart(after)
}

// --- status ---

type statusView struct {
	Version    string         `json:"version"`
	Configured bool           `json:"configured"`
	Missing    []string       `json:"missing"`
	Running    bool           `json:"running"`
	Error      string         `json:"error,omitempty"` // why the bridge did not start
	Name       string         `json:"name"`
	Model      string         `json:"model"`
	MAC        string         `json:"mac"`
	Bridge     *bridge.Status `json:"bridge,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := s.opts.Store.Get()
	v := statusView{
		Version:    s.opts.Version,
		Configured: cfg.Complete(),
		Missing:    cfg.Missing(),
		Name:       cfg.Camera.Name,
		MAC:        cfg.Identity.MAC,
		Model:      cfg.Camera.Model,
	}
	if id, ok := unifi.Identities[cfg.Camera.Model]; ok {
		v.Model = strings.TrimPrefix(id.Model, "UVC ")
	}
	if s.opts.Runner != nil {
		b, startErr := s.opts.Runner.Current()
		v.Error = startErr
		if b != nil {
			st := b.Status()
			v.Running, v.Bridge = true, &st
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > logbuf.Size {
		n = 200
	}
	lines := []string{}
	if s.opts.Logs != nil {
		lines = s.opts.Logs.Lines(n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	var b *bridge.Bridge
	if s.opts.Runner != nil {
		b, _ = s.opts.Runner.Current()
	}
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, "the bridge is not running")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	jpeg, err := b.Snapshot(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "snapshot: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
	_, _ = w.Write(jpeg)
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "doorbell" && kind != "protect" {
		writeError(w, http.StatusBadRequest, "kind must be doorbell or protect")
		return
	}
	if !s.discover.TryLock() {
		writeError(w, http.StatusTooManyRequests, "a search is already running")
		return
	}
	defer s.discover.Unlock()

	s.opts.Log.Info().Str("kind", kind).Msg("[web] searching the local network")
	var (
		devices any
		err     error
	)
	if kind == "doorbell" {
		devices, err = discovery.Doorbells(r.Context())
	} else {
		devices, err = discovery.Consoles(r.Context(), 3*time.Second)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}
