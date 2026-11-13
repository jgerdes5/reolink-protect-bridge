// Package web is the bridge's settings and status UI: a small JSON API and an
// embedded single-page app.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
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
	// Ingress: running as a Home Assistant add-on, reachable only through
	// Home Assistant's Ingress proxy, which already authenticated the user;
	// no own login, and the page is shown in Home Assistant's frame.
	Ingress bool
	// IngressPeer is the only client address accepted in Ingress mode
	// (default DefaultIngressPeer, the Supervisor's address).
	IngressPeer string
}

// DefaultIngressPeer is the address Home Assistant's Supervisor connects
// from for Ingress. Anything else that reaches the listening address (other
// add-ons on the hassio network, the host itself) would otherwise get the
// page without a login.
const DefaultIngressPeer = "172.30.32.2"

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
	if opts.Ingress && opts.IngressPeer == "" {
		opts.IngressPeer = DefaultIngressPeer
	}
	sessionFile := filepath.Join(filepath.Dir(opts.Store.Path()), "sessions.json")
	s := &Server{opts: opts, sessions: newSessions(sessionFile), limiter: newLimiter(), mux: http.NewServeMux()}

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
	s.mux.HandleFunc("POST /api/password", s.auth(s.handlePassword))
	s.mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	s.mux.HandleFunc("GET /api/logs", s.auth(s.handleLogs))
	s.mux.HandleFunc("GET /api/logs.txt", s.auth(s.handleLogFile))
	s.mux.HandleFunc("GET /api/discover", s.auth(s.handleDiscover))
	s.mux.HandleFunc("GET /api/snapshot.jpg", s.auth(s.handleSnapshot))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	frames := "'none'"
	if s.opts.Ingress {
		frames = "'self'" // Home Assistant shows Ingress pages in a same-origin frame
	} else {
		h.Set("X-Frame-Options", "DENY")
	}
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; style-src 'self'; script-src 'self'; frame-ancestors "+frames+"; base-uri 'none'; form-action 'self'")
	if s.opts.Ingress && clientIP(r) != s.opts.IngressPeer {
		// Without its own login the page may only be served to the proxy
		// that authenticated the user.
		writeError(w, http.StatusForbidden, "only reachable through Home Assistant")
		return
	}
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
		if !s.opts.Ingress && !s.sessions.valid(sessionToken(r)) {
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
	if s.opts.Ingress {
		writeJSON(w, http.StatusOK, map[string]any{
			"setup": false, "authenticated": true, "ingress": true, "version": s.opts.Version,
		})
		return
	}
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
	if s.opts.Ingress {
		writeError(w, http.StatusConflict, "logins are handled by Home Assistant")
		return
	}
	if !s.setupHostAllowed(r.Host) {
		// DNS rebinding: a web site whose name suddenly resolves to this
		// host could otherwise create the admin password before the owner.
		writeError(w, http.StatusForbidden, "to create the admin password, open this page by the IP address of the machine it runs on (http://IP:port) or by a local name such as bridge.local, not by a public domain name")
		return
	}
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

// setupHostAllowed accepts the Host header of a page opened by IP address,
// as localhost, by a single-label name or a name in a local-only domain, or
// by the host of the configured listen address. Public names are refused:
// a DNS rebinding attack needs one.
func (s *Server) setupHostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".home.arpa", ".internal", ".localhost"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if lh, _, err := net.SplitHostPort(s.opts.Store.Get().Web.Listen); err == nil && lh != "" {
		return strings.EqualFold(strings.TrimSuffix(lh, "."), host)
	}
	return false
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in passwordInput
	if !readJSON(w, r, &in) {
		return
	}
	cfg := s.opts.Store.Get()
	if !cfg.HasAdmin() {
		writeError(w, http.StatusConflict, "create the admin password first")
		return
	}
	ip := clientIP(r)
	if d := s.limiter.reserve(ip); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed logins, try again in "+d.Round(time.Second).String())
		return
	}
	if !cfg.CheckAdminPassword(in.Password) {
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
		CertPin  string `json:"certPin"` // not secret: the key the console showed
	} `json:"protect"`
	Camera   config.Camera `json:"camera"`
	Identity struct {
		MAC string `json:"mac"`
	} `json:"identity"`
	Advanced advanced `json:"advanced"`
	Ingress  bool     `json:"ingress"`
}

type advanced struct {
	Listen string `json:"listen"` // web page address; takes effect after an app restart
	FFmpeg string `json:"ffmpeg"`
}

func viewOf(c config.Config) configView {
	var v configView
	v.Doorbell.Host, v.Doorbell.Port, v.Doorbell.Username, v.Doorbell.Channel = c.Doorbell.Host, c.Doorbell.Port, c.Doorbell.Username, c.Doorbell.Channel
	v.Doorbell.Password.Set = c.Doorbell.Password != ""
	v.Protect.Host, v.Protect.Username = c.Protect.Host, c.Protect.Username
	v.Protect.Password.Set = c.Protect.Password != ""
	v.Protect.Token.Set = c.Protect.Token != ""
	v.Protect.CertPin = c.Protect.CertPin
	v.Camera = c.Camera
	v.Identity.MAC = c.Identity.MAC
	v.Advanced = advanced{Listen: c.Web.Listen, FFmpeg: c.FFmpeg}
	return v
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.view(s.opts.Store.Get()))
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
		ClearCertPin  bool   `json:"clearCertPin"` // trust the certificate the console shows next
	} `json:"protect"`
	Camera   *config.Camera `json:"camera"`
	Advanced *advanced      `json:"advanced"`
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var in configInput
	if !readJSON(w, r, &in) {
		return
	}
	if a := in.Advanced; a != nil {
		// checked before taking the store lock: it touches the file system
		ffmpeg := strings.TrimSpace(a.FFmpeg)
		if ffmpeg == "" {
			ffmpeg = "ffmpeg"
		}
		if err := config.CheckFFmpeg(ffmpeg); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	before := s.opts.Store.Get()
	cfg, err := s.opts.Store.Update(func(c *config.Config) error {
		// Stored secrets are only ever sent to the address they were entered
		// for: if the address changes they must be entered again, otherwise
		// whoever can change the address could collect them.
		if d := in.Doorbell; d != nil {
			host := strings.TrimSpace(d.Host)
			if host != "" && host != c.Doorbell.Host && c.Doorbell.Password != "" && d.Password == "" {
				return errReenter("doorbell address", "the doorbell password")
			}
			c.Doorbell.Host = host
			c.Doorbell.Port = d.Port
			c.Doorbell.Username = strings.TrimSpace(d.Username)
			c.Doorbell.Channel = d.Channel
			if d.Password != "" {
				c.Doorbell.Password = d.Password
			}
		}
		if p := in.Protect; p != nil {
			host := strings.TrimSpace(p.Host)
			if host != "" && host != c.Protect.Host {
				if c.Protect.Password != "" && p.Password == "" && !p.ClearPassword && strings.TrimSpace(p.Username) != "" {
					return errReenter("Protect console address", "the Protect password")
				}
				if c.Protect.Token != "" && strings.TrimSpace(p.Token) == "" && !p.ClearToken {
					return errReenter("Protect console address", "the adoption token (or remove it)")
				}
			}
			if host != c.Protect.Host || p.ClearCertPin {
				c.Protect.CertPin = "" // learned again on the next login
			}
			c.Protect.Host = host
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
		if a := in.Advanced; a != nil {
			listen := strings.TrimSpace(a.Listen)
			if s.opts.Ingress {
				listen = c.Web.Listen // not used behind Ingress, and not editable there
			}
			if err := config.ValidListen(listen); err != nil {
				return err
			}
			ffmpeg := strings.TrimSpace(a.FFmpeg)
			if ffmpeg == "" {
				ffmpeg = "ffmpeg"
			}
			c.Web.Listen, c.FFmpeg = listen, ffmpeg
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.applied(before, cfg)
	writeJSON(w, http.StatusOK, s.view(cfg))
}

// errReenter is the error for a changed address whose stored secret was not
// entered again.
func errReenter(field, secret string) error {
	return fmt.Errorf("the %s changed: enter %s again (stored passwords are only sent to the address they were entered for)", field, secret)
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
	writeJSON(w, http.StatusOK, s.view(cfg))
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
	s.opts.Runner.Restart() // with the latest configuration, also if another save came in between
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
		Model:      strings.TrimPrefix(unifi.G4Doorbell.Model, "UVC "),
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

// handleLogFile downloads every kept line (the page shows the newest ones).
func (s *Server) handleLogFile(w http.ResponseWriter, _ *http.Request) {
	var lines []string
	if s.opts.Logs != nil {
		lines = s.opts.Logs.Lines(0)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="reolink-protect-bridge.log"`)
	w.Header().Set("Cache-Control", "no-store")
	for _, l := range lines {
		_, _ = io.WriteString(w, l+"\n")
	}
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

type passwordChange struct {
	Current  string `json:"current"`
	Password string `json:"password"`
}

// handlePassword changes the admin password; other sessions are logged out.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if s.opts.Ingress {
		writeError(w, http.StatusConflict, "logins are handled by Home Assistant")
		return
	}
	var in passwordChange
	if !readJSON(w, r, &in) {
		return
	}
	ip := clientIP(r)
	if wait := s.limiter.reserve(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("too many attempts, try again in %s", wait.Round(time.Second)))
		return
	}
	if !s.opts.Store.Get().CheckAdminPassword(in.Current) {
		writeError(w, http.StatusForbidden, "the current password is wrong")
		return
	}
	s.limiter.success(ip)
	if _, err := s.opts.Store.Update(func(c *config.Config) error { return c.SetAdminPassword(in.Password) }); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.sessions.clear()
	s.opts.Log.Info().Msg("[web] admin password changed")
	s.startSession(w, r)
}

func (s *Server) view(c config.Config) configView {
	v := viewOf(c)
	v.Ingress = s.opts.Ingress
	return v
}
