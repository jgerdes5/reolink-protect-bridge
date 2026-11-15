package unifi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	// CertPin is the SHA-256 (hex) of the console's public key; empty means
	// learn it on the first login and report it through OnPin. See verify.
	CertPin string
	OnPin   func(pin string)

	pinMu sync.Mutex

	mu        sync.Mutex // one login session at a time
	client    *http.Client
	transport *http.Transport
	csrf      string
	loggedIn  time.Time // session reused by Ring
}

// PinError is a console certificate that differs from the pinned one.
type PinError struct {
	Got, Want string
	Camera    bool // on the camera connection (7442), not the login
}

func (e *PinError) Error() string {
	if e.Camera {
		return fmt.Sprintf("the certificate of Protect's camera connection changed (key %s, trusted %s): not connecting; "+
			"if the console or Protect was reset or replaced, trust the new certificate on the Connections page", short(e.Got), short(e.Want))
	}
	return fmt.Sprintf("the console's certificate changed (key %s, trusted %s): refusing to send the password; "+
		"if you replaced the console's certificate, trust the new one on the Connections page", short(e.Got), short(e.Want))
}

func short(pin string) string {
	if len(pin) > 16 {
		return pin[:16] + "…"
	}
	return pin
}

// CertPinOf is the pin of a certificate: SHA-256 of its public key, so a
// renewed certificate with the same key still matches.
func CertPinOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// verify runs on every TLS connection to the console, before the password
// is sent. A certificate valid for the host name under the system roots
// (e.g. Let's Encrypt) is accepted as it is. Any other (the self-signed
// default) must match the pinned key; without a pin the first one seen is
// trusted and pinned (trust on first use).
func (p *ProtectClient) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("console sent no certificate")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: inter}); err == nil && cs.ServerName != "" {
		return nil
	}

	pin := CertPinOf(leaf)
	p.pinMu.Lock()
	want := p.CertPin
	if want == "" {
		p.CertPin = pin
	}
	onPin := p.OnPin
	p.pinMu.Unlock()
	switch {
	case want == "":
		if onPin != nil {
			onPin(pin)
		}
		return nil
	case want != pin:
		return &PinError{Got: pin, Want: want}
	}
	return nil
}

// sessionMaxAge is how long Ring reuses a login before logging in again.
const sessionMaxAge = 30 * time.Minute

func (p *ProtectClient) base() string {
	if ip := net.ParseIP(p.Host); ip != nil && ip.To4() == nil {
		return "https://[" + p.Host + "]" // bare IPv6
	}
	return "https://" + p.Host
}

func (p *ProtectClient) login(ctx context.Context) error {
	// A fresh cookie jar per login, one transport for the client's lifetime.
	if p.transport == nil {
		p.transport = &http.Transport{
			// Consoles mostly use self-signed certificates: verify checks a
			// publicly trusted one normally and pins any other.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyConnection: p.verify},
			IdleConnTimeout: 30 * time.Second,
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second, Transport: p.transport}

	body, _ := json.Marshal(map[string]any{
		"username": p.Username, "password": p.Password, "rememberMe": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base()+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	p.setOrigin(req)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("protect login: HTTP %d", resp.StatusCode)
	}

	// keep the session only once it is known to work
	p.client = client
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
		fresh := false
		if p.client == nil || time.Since(p.loggedIn) > sessionMaxAge || attempt > 0 {
			if err := p.login(ctx); err != nil {
				return err
			}
			fresh = true
		}
		err := p.ring(ctx, mac)
		var status *statusError
		if !fresh && errors.As(err, &status) && (status.code == 401 || status.code == 403) {
			continue // the reused session expired: one fresh login
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
