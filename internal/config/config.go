// Package config is the bridge's only settings file: one JSON document in the
// data directory, written atomically with mode 0600.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jgerdes5/reolink-protect-bridge/internal/bridge"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"golang.org/x/crypto/bcrypt"
)

// FileName is the settings file inside the data directory.
const FileName = "config.json"

type Doorbell struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"` // 0 = 9000
	Username string `json:"username"`
	Password string `json:"password"`
	Channel  int    `json:"channel,omitempty"`
}

type Protect struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

type Camera struct {
	Name     string `json:"name"`
	Ring     string `json:"ring"` // "camera" or "api"
	Events   bool   `json:"events"`
	Talkback bool   `json:"talkback"`
	Settings bool   `json:"settings"` // apply Protect's camera settings to the doorbell
}

// Identity is what Protect knows the camera by. It is generated once and only
// replaced on request: a new one is a new camera for Protect.
type Identity struct {
	MAC  string `json:"mac"`
	Cert string `json:"cert"`
}

type Web struct {
	Listen string `json:"listen"`
}

type Admin struct {
	PasswordHash string `json:"passwordHash"`
}

// Config is the whole settings file.
type Config struct {
	Doorbell Doorbell `json:"doorbell"`
	Protect  Protect  `json:"protect"`
	Camera   Camera   `json:"camera"`
	Identity Identity `json:"identity"`
	Web      Web      `json:"web"`
	Admin    Admin    `json:"admin"`
	FFmpeg   string   `json:"ffmpeg"`
}

// Default is a fresh configuration without identity.
func Default() Config {
	return Config{
		Doorbell: Doorbell{Username: "admin"},
		Camera: Camera{
			Name:     "Doorbell",
			Ring:     "camera",
			Events:   true,
			Talkback: true,
			Settings: true,
		},
		Web:    Web{Listen: ":8080"},
		FFmpeg: "ffmpeg",
	}
}

// fillDefaults repairs empty fields a hand-edited file may leave behind.
func (c *Config) fillDefaults() {
	d := Default()
	if c.Camera.Name == "" {
		c.Camera.Name = d.Camera.Name
	}
	if c.Camera.Ring == "" {
		c.Camera.Ring = d.Camera.Ring
	}
	if c.Web.Listen == "" {
		c.Web.Listen = d.Web.Listen
	}
	if c.FFmpeg == "" {
		c.FFmpeg = d.FFmpeg
	}
}

// NewIdentity generates a new MAC and TLS certificate.
func NewIdentity() (Identity, error) {
	seed := make([]byte, 16)
	if _, err := rand.Read(seed); err != nil {
		return Identity{}, err
	}
	cert, err := unifi.GenerateCert()
	if err != nil {
		return Identity{}, err
	}
	return Identity{MAC: bridge.NewMAC(hex.EncodeToString(seed)), Cert: cert}, nil
}

// Missing lists what must still be filled in before the bridge can run.
func (c Config) Missing() []string {
	var m []string
	if c.Doorbell.Host == "" {
		m = append(m, "doorbell address")
	}
	if c.Doorbell.Username == "" {
		m = append(m, "doorbell user")
	}
	if c.Doorbell.Password == "" {
		m = append(m, "doorbell password")
	}
	if c.Protect.Host == "" {
		m = append(m, "Protect console address")
	}
	switch {
	case c.Protect.Username == "" && c.Protect.Token == "":
		m = append(m, "Protect user or adoption token")
	case c.Protect.Username != "" && c.Protect.Password == "":
		m = append(m, "Protect password")
	}
	if c.Camera.Ring == "api" && c.Protect.Username == "" {
		m = append(m, `Protect user (ring method "Protect API" needs it)`)
	}
	return m
}

// Complete reports whether the bridge can start.
func (c Config) Complete() bool { return len(c.Missing()) == 0 }

// Validate checks the values that are set; empty connection fields are
// allowed (the config is then just incomplete).
func (c Config) Validate() error {
	if err := validHost(c.Doorbell.Host); err != nil {
		return fmt.Errorf("doorbell address: %w", err)
	}
	if err := validHost(c.Protect.Host); err != nil {
		return fmt.Errorf("Protect address: %w", err)
	}
	if c.Doorbell.Port < 0 || c.Doorbell.Port > 65535 {
		return errors.New("doorbell port must be 1-65535")
	}
	if c.Doorbell.Channel < 0 || c.Doorbell.Channel > 255 {
		return errors.New("doorbell channel must be 0-255")
	}
	name := strings.TrimSpace(c.Camera.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return errors.New("camera name must be 1-64 characters")
	}
	if c.Camera.Ring != "camera" && c.Camera.Ring != "api" {
		return fmt.Errorf("unknown ring method %q", c.Camera.Ring)
	}
	if c.Identity.MAC != "" {
		if _, err := bridge.NormalizeMAC(c.Identity.MAC); err != nil {
			return err
		}
	}
	return nil
}

// validHost accepts a host name or IP address, optionally with a port, but no
// scheme or path.
func validHost(h string) error {
	if h == "" {
		return nil
	}
	if strings.ContainsAny(h, "/ \t\r\n@?#") {
		return errors.New("enter just a host name or IP address")
	}
	host := h
	if hh, _, err := net.SplitHostPort(h); err == nil {
		host = hh
	}
	if host == "" {
		return errors.New("empty host")
	}
	if _, err := url.Parse("//" + h); err != nil {
		return errors.New("invalid host")
	}
	return nil
}

// Options maps the configuration to bridge options.
func (c Config) Options() bridge.Options {
	return bridge.Options{
		Doorbell: bridge.Doorbell{
			Host:     c.Doorbell.Host,
			Port:     c.Doorbell.Port,
			Username: c.Doorbell.Username,
			Password: c.Doorbell.Password,
			Channel:  c.Doorbell.Channel,
		},
		Protect: bridge.Protect{
			Host:     c.Protect.Host,
			Username: c.Protect.Username,
			Password: c.Protect.Password,
			Token:    c.Protect.Token,
		},
		Name:     strings.TrimSpace(c.Camera.Name),
		MAC:      c.Identity.MAC,
		Cert:     c.Identity.Cert,
		Ring:     c.Camera.Ring,
		Events:   c.Camera.Events,
		Talkback: c.Camera.Talkback,
		Settings: c.Camera.Settings,
		FFmpeg:   c.FFmpeg,
	}
}

// Secrets are the values that must never appear in logs.
func (c Config) Secrets() []string {
	var s []string
	for _, v := range []string{c.Doorbell.Password, c.Protect.Password, c.Protect.Token} {
		if v != "" {
			s = append(s, v)
		}
	}
	return s
}

// HasAdmin reports whether the admin password was created.
func (c Config) HasAdmin() bool { return c.Admin.PasswordHash != "" }

// Password limits; bcrypt only uses the first 72 bytes.
const (
	MinPassword = 8
	MaxPassword = 72
)

// SetAdminPassword stores a bcrypt hash of password.
func (c *Config) SetAdminPassword(password string) error {
	if utf8.RuneCountInString(password) < MinPassword {
		return fmt.Errorf("password must have at least %d characters", MinPassword)
	}
	if len(password) > MaxPassword {
		return fmt.Errorf("password must be at most %d bytes", MaxPassword)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	c.Admin.PasswordHash = string(hash)
	return nil
}

// CheckAdminPassword compares password with the stored hash.
func (c Config) CheckAdminPassword(password string) bool {
	if c.Admin.PasswordHash == "" || len(password) > MaxPassword {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(c.Admin.PasswordHash), []byte(password)) == nil
}

// Store holds the configuration in memory and on disk.
type Store struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

// Open loads dir/config.json, creating the directory, the file and the camera
// identity on first start.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, FileName), cfg: Default()}

	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err = json.Unmarshal(data, &s.cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
		s.cfg.fillDefaults()
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}

	dirty := err != nil
	if s.cfg.Identity.MAC == "" || s.cfg.Identity.Cert == "" {
		id, err := NewIdentity()
		if err != nil {
			return nil, err
		}
		s.cfg.Identity = id
		dirty = true
	}
	if dirty {
		if err := save(s.path, &s.cfg); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Path is the settings file.
func (s *Store) Path() string { return s.path }

// Get returns a copy of the configuration.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Update changes a copy of the configuration, validates and saves it. Nothing
// changes if fn, validation or saving fails.
func (s *Store) Update(fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	if err := fn(&cfg); err != nil {
		return s.cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return s.cfg, err
	}
	if err := save(s.path, &cfg); err != nil {
		return s.cfg, err
	}
	s.cfg = cfg
	return cfg, nil
}

// save writes the file atomically: a temporary file in the same directory,
// synced, then renamed over the old one.
func save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	f, err := os.CreateTemp(filepath.Dir(path), "."+FileName+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	if err = f.Chmod(0o600); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
