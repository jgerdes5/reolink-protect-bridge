package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestOpenRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	require.True(t, s.Created())

	s2, err := Open(dir)
	require.NoError(t, err)
	require.False(t, s2.Created())

	// a hand-edited invalid value is reported, not silently replaced
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	bad := strings.Replace(string(data), `"ring": "camera"`, `"ring": "bell"`, 1)
	require.NotEqual(t, string(data), bad)
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))
	_, err = Open(dir)
	require.ErrorContains(t, err, "ring method")
	require.ErrorContains(t, err, path)

	// empty values with a safe default are filled in
	empty := strings.Replace(string(data), `"ring": "camera"`, `"ring": ""`, 1)
	require.NoError(t, os.WriteFile(path, []byte(empty), 0o600))
	s3, err := Open(dir)
	require.NoError(t, err)
	require.Equal(t, "camera", s3.Get().Camera.Ring)
}

func TestFFmpegPath(t *testing.T) {
	cfg := Default()
	cfg.FFmpeg = "/bin/sh"
	require.Error(t, cfg.Validate())
	cfg.FFmpeg = "/usr/bin/ffmpeg"
	require.NoError(t, cfg.Validate())

	require.NoError(t, CheckFFmpeg("ffmpeg"))
	require.Error(t, CheckFFmpeg("sh"))
	require.Error(t, CheckFFmpeg("/bin/sh"))
	require.Error(t, CheckFFmpeg(filepath.Join(t.TempDir(), "ffmpeg"))) // missing

	dir := t.TempDir()
	exe := filepath.Join(dir, "ffmpeg")
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o600))
	require.Error(t, CheckFFmpeg(exe)) // not executable
	require.NoError(t, os.Chmod(exe, 0o755))
	require.NoError(t, CheckFFmpeg(exe))
	sub := filepath.Join(t.TempDir(), "ffmpeg")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.ErrorContains(t, CheckFFmpeg(sub), "not an executable file")
}
