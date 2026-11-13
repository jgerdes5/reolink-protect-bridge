package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenCreatesIdentityOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	require.NoError(t, err)

	cfg := s.Get()
	require.Len(t, cfg.Identity.MAC, 12)
	require.Equal(t, "02", cfg.Identity.MAC[:2])
	require.Contains(t, cfg.Identity.Cert, "BEGIN CERTIFICATE")
	require.Equal(t, "Doorbell", cfg.Camera.Name)
	require.True(t, cfg.Camera.Events && cfg.Camera.Talkback && cfg.Camera.Settings)
	require.False(t, cfg.Complete())

	fi, err := os.Stat(filepath.Join(dir, FileName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	s2, err := Open(dir)
	require.NoError(t, err)
	require.Equal(t, cfg.Identity, s2.Get().Identity)
}

func TestUpdateValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)

	_, err = s.Update(func(c *Config) error {
		c.Doorbell.Host = "http://x/"
		return nil
	})
	require.Error(t, err)
	require.Empty(t, s.Get().Doorbell.Host)

	_, err = s.Update(func(c *Config) error { return errors.New("no") })
	require.Error(t, err)

	cfg, err := s.Update(func(c *Config) error {
		c.Doorbell.Host = "192.168.1.20"
		c.Doorbell.Password = "pw"
		c.Protect.Host = "unifi.local:443"
		c.Protect.Token = "tok"
		c.Camera.Events = false
		return nil
	})
	require.NoError(t, err)
	require.True(t, cfg.Complete(), cfg.Missing())

	opts := cfg.Options()
	require.Equal(t, "192.168.1.20", opts.Doorbell.Host)
	require.Equal(t, "tok", opts.Protect.Token)
	require.False(t, opts.Events)
	require.True(t, opts.Talkback)
	require.Equal(t, cfg.Identity.MAC, opts.MAC)

	s2, err := Open(dir)
	require.NoError(t, err)
	require.False(t, s2.Get().Camera.Events)
	require.Equal(t, []string{"pw", "tok"}, s2.Get().Secrets())

	matches, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	require.Empty(t, matches)
}

func TestRingAPINeedsUser(t *testing.T) {
	cfg := Default()
	cfg.Doorbell = Doorbell{Host: "a", Username: "admin", Password: "p"}
	cfg.Protect = Protect{Host: "b", Token: "t"}
	require.True(t, cfg.Complete())
	cfg.Camera.Ring = "api"
	require.False(t, cfg.Complete())
}

func TestAdminPassword(t *testing.T) {
	cfg := Default()
	require.Error(t, cfg.SetAdminPassword("short"))
	require.NoError(t, cfg.SetAdminPassword("long enough"))
	require.True(t, cfg.CheckAdminPassword("long enough"))
	require.False(t, cfg.CheckAdminPassword("wrong pass"))
}
