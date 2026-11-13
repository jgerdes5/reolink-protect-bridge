package web

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	cookieName    = "rpb_session"
	sessionMaxAge = 7 * 24 * time.Hour
)

// sessions is the server-side session store (in memory: a restart logs out).
type sessions struct {
	mu   sync.Mutex
	byID map[string]time.Time // token -> expiry
}

func newSessions() *sessions { return &sessions{byID: map[string]time.Time{}} }

func (s *sessions) create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for t, exp := range s.byID {
		if now.After(exp) {
			delete(s.byID, t)
		}
	}
	s.byID[token] = now.Add(sessionMaxAge)
	return token, nil
}

func (s *sessions) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.byID[token]
	if ok && time.Now().After(exp) {
		delete(s.byID, token)
		return false
	}
	return ok
}

func (s *sessions) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, token)
}

func (s *sessions) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.byID)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionMaxAge / time.Second),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// limiter slows down password guessing per client address: after 5 failures
// each further attempt is blocked for a doubling time, up to 15 minutes.
type limiter struct {
	mu    sync.Mutex
	fails map[string]*failState
}

type failState struct {
	count int
	until time.Time
	last  time.Time
}

func newLimiter() *limiter { return &limiter{fails: map[string]*failState{}} }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked returns how long the client must still wait.
func (l *limiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.fails[ip]; f != nil {
		if d := time.Until(f.until); d > 0 {
			return d
		}
	}
	return 0
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, f := range l.fails { // forget old entries
		if now.Sub(f.last) > time.Hour {
			delete(l.fails, k)
		}
	}
	f := l.fails[ip]
	if f == nil {
		f = &failState{}
		l.fails[ip] = f
	}
	f.count++
	f.last = now
	if f.count >= 5 {
		d := time.Second << min(f.count-5, 10)
		f.until = now.Add(min(d, 15*time.Minute))
	}
}

func (l *limiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
