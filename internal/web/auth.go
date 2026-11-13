package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	cookieName    = "rpb_session"
	sessionMaxAge = 7 * 24 * time.Hour
)

// sessions is the server-side session store. It is kept in a file next to the
// config so a restart does not log everyone out; only SHA-256 hashes of the
// tokens are stored, so the file does not contain usable cookies.
type sessions struct {
	mu   sync.Mutex
	byID map[string]time.Time // token hash -> expiry
	path string               // "" = memory only
}

func newSessions(path string) *sessions {
	s := &sessions{byID: map[string]time.Time{}, path: path}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &s.byID)
		}
	}
	return s
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// save writes the store; must be called with s.mu held.
func (s *sessions) save() {
	if s.path == "" {
		return
	}
	b, _ := json.Marshal(s.byID)
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sessions.*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o600) != nil || os.Rename(tmp.Name(), s.path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

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
	s.byID[hashToken(token)] = now.Add(sessionMaxAge)
	s.save()
	return token, nil
}

func (s *sessions) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := hashToken(token)
	exp, ok := s.byID[h]
	if ok && time.Now().After(exp) {
		delete(s.byID, h)
		s.save()
		return false
	}
	return ok
}

func (s *sessions) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, hashToken(token))
	s.save()
}

func (s *sessions) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.byID)
	s.save()
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
// Usage: reserve before checking the password, success if it was right.
type limiter struct {
	mu    sync.Mutex
	fails map[string]*failState
	now   func() time.Time // for tests
}

type failState struct {
	count int
	until time.Time
	last  time.Time
}

func newLimiter() *limiter { return &limiter{fails: map[string]*failState{}, now: time.Now} }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// reserve atomically checks whether the client may try a password now and,
// if so, counts the attempt as a failure in advance; success removes it
// again. Checking and counting in one step means parallel requests cannot all
// pass the check before the first failure is recorded. It returns how long the
// client must still wait (0: go ahead).
func (l *limiter) reserve(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
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
	if d := f.until.Sub(now); d > 0 {
		return d
	}
	f.count++
	f.last = now
	if f.count >= 5 {
		d := time.Second << min(f.count-5, 10)
		f.until = now.Add(min(d, 15*time.Minute))
	}
	return 0
}

func (l *limiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}
