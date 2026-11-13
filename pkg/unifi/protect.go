package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
)

// ProtectClient is a minimal client for the UniFi OS / Protect web API, logged in
// with a local (non-SSO, no MFA) account.
type ProtectClient struct {
	Host     string
	Username string
	Password string

	mu        sync.Mutex // one login session at a time
	client    *http.Client
	transport *http.Transport
	csrf      string
	loggedIn  time.Time // session reused by Ring
}

// sessionMaxAge is how long Ring reuses a login before logging in again.
const sessionMaxAge = 30 * time.Minute

func (p *ProtectClient) base() string {
	return "https://" + p.Host
}

func (p *ProtectClient) login(ctx context.Context) error {
	// A fresh cookie jar per login, one transport for the client's lifetime.
	if p.transport == nil {
		p.transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed console cert
			IdleConnTimeout: 30 * time.Second,
		}
	}
	jar, _ := cookiejar.New(nil)
	p.client = &http.Client{Jar: jar, Timeout: 15 * time.Second, Transport: p.transport}

	body, _ := json.Marshal(map[string]any{
		"username": p.Username, "password": p.Password, "rememberMe": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base()+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	p.setOrigin(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("protect login: HTTP %d", resp.StatusCode)
	}

	p.csrf = resp.Header.Get("X-CSRF-Token")
	if p.csrf == "" {
		p.csrf = resp.Header.Get("X-Updated-CSRF-Token")
	}
	p.loggedIn = time.Now()
	return nil
}

func (p *ProtectClient) setOrigin(req *http.Request) {
	req.Header.Set("Origin", p.base())
	req.Header.Set("Referer", p.base()+"/")
}

// AdoptionToken logs in and returns a fresh adoption token from
// /proxy/protect/api/cameras/manage-payload (valid about an hour).
func (p *ProtectClient) AdoptionToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.login(ctx); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base()+"/proxy/protect/api/cameras/manage-payload", nil)
	if err != nil {
		return "", err
	}
	p.setOrigin(req)
	if p.csrf != "" {
		req.Header.Set("X-CSRF-Token", p.csrf)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("protect manage-payload: HTTP %d", resp.StatusCode)
	}

	var v struct {
		Mgmt struct {
			Token string `json:"token"`
		} `json:"mgmt"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.Mgmt.Token == "" {
		return "", errors.New("protect manage-payload: no token")
	}
	return v.Mgmt.Token, nil
}

// Ring creates a doorbell ring event for the camera with this MAC through the
// route UniFi Access doorbells use (POST /proxy/protect/api/events/access/action,
// action "door_bell"), as unifi-cam-proxy does. The user needs write permission
// on the camera. The camera protocol's own ring (Camera.Ring) needs no login.
func (p *ProtectClient) Ring(ctx context.Context, mac string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Reuse the session (UniFi OS rate-limits logins); log in again when it is
	// old or refused.
	for attempt := 0; ; attempt++ {
		if p.client == nil || time.Since(p.loggedIn) > sessionMaxAge || attempt > 0 {
			if err := p.login(ctx); err != nil {
				return err
			}
		}
		err := p.ring(ctx, mac)
		var status *statusError
		if attempt == 0 && errors.As(err, &status) && (status.code == 401 || status.code == 403) {
			continue
		}
		return err
	}
}

func (p *ProtectClient) ring(ctx context.Context, mac string) error {
	var cams []struct {
		ID  string `json:"id"`
		MAC string `json:"mac"`
	}
	if err := p.do(ctx, http.MethodGet, "/proxy/protect/api/cameras", nil, &cams); err != nil {
		return err
	}
	id := ""
	for _, cam := range cams {
		if strings.EqualFold(strings.ReplaceAll(cam.MAC, ":", ""), mac) {
			id = cam.ID
		}
	}
	if id == "" {
		return fmt.Errorf("protect: camera %s not found", mac)
	}

	now := time.Now().UnixMilli()
	return p.do(ctx, http.MethodPost, "/proxy/protect/api/events/access/action", map[string]any{
		"action": "door_bell", "camera_id": id, "start_time": now, "end_time": now + 3000,
	}, nil)
}

func (p *ProtectClient) do(ctx context.Context, method, path string, in, out any) error {
	var body *bytes.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base()+path, body)
	if err != nil {
		return err
	}
	p.setOrigin(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.csrf != "" {
		req.Header.Set("X-CSRF-Token", p.csrf)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &statusError{method: method, path: path, code: resp.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type statusError struct {
	method, path string
	code         int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("protect %s %s: HTTP %d", e.method, e.path, e.code)
}
