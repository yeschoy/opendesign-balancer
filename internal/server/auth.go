package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"opendesign-balancer/internal/fsutil"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iter    = 600000
	sessionCookie = "odb_session"
	sessionTTL    = 24 * time.Hour
)

func hashPassword(pw string) (string, error) {
	if len(pw) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

type adminSession struct {
	csrf    string
	expires time.Time
}

type adminAuth struct {
	mu       sync.Mutex
	sessions map[string]*adminSession // by sha256 of the cookie token, so the file never holds a usable token
	fails    map[string][]time.Time
	path     string // sessions file; empty: sessions live in memory only
}

// newAdminAuth makes the login state; with a path, sessions survive restarts
// (a redeploy would otherwise log everyone out).
func newAdminAuth(path string) *adminAuth {
	a := &adminAuth{sessions: map[string]*adminSession{}, fails: map[string][]time.Time{}, path: path}
	if path == "" {
		return a
	}
	var saved map[string]struct {
		CSRF    string    `json:"csrf"`
		Expires time.Time `json:"expires"`
	}
	if fsutil.ReadJSON(path, &saved) == nil {
		now := time.Now()
		for k, v := range saved {
			if v.Expires.After(now) && v.CSRF != "" {
				a.sessions[k] = &adminSession{csrf: v.CSRF, expires: v.Expires}
			}
		}
	}
	return a
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// save writes the sessions file (caller holds the lock). A failed write only
// costs a re-login after the next restart.
func (a *adminAuth) save() {
	if a.path == "" {
		return
	}
	out := map[string]any{}
	for k, s := range a.sessions {
		out[k] = map[string]any{"csrf": s.csrf, "expires": s.expires}
	}
	_ = fsutil.WriteJSONCompactAtomic(a.path, out)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limited: 5 failed logins per IP per 60 s.
func (a *adminAuth) limited(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	kept := a.fails[ip][:0]
	for _, t := range a.fails[ip] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	a.fails[ip] = kept
	return len(kept) >= 5
}

func (a *adminAuth) fail(ip string) {
	a.mu.Lock()
	a.fails[ip] = append(a.fails[ip], time.Now())
	a.mu.Unlock()
}

func (a *adminAuth) create(w http.ResponseWriter, r *http.Request) *adminSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
	}
	token := randHex(32)
	s := &adminSession{csrf: randHex(16), expires: now.Add(sessionTTL)}
	a.sessions[tokenKey(token)] = s
	a.save()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r), MaxAge: int(sessionTTL.Seconds())})
	return s
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *adminAuth) get(r *http.Request) (string, *adminSession) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[tokenKey(c.Value)]
	if s == nil || time.Now().After(s.expires) {
		return "", nil
	}
	return c.Value, s
}

func (a *adminAuth) destroy(token string) {
	a.mu.Lock()
	delete(a.sessions, tokenKey(token))
	a.save()
	a.mu.Unlock()
}

// destroyAll logs everyone out (password change).
func (a *adminAuth) destroyAll() {
	a.mu.Lock()
	a.sessions = map[string]*adminSession{}
	a.save()
	a.mu.Unlock()
}

// sameOrigin: a present Origin header must name this host.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return strings.EqualFold(u.Host, host)
}

// adminOnly requires a session, and for writes the CSRF token and origin.
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, sess := s.admin.get(r)
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.csrf)) != 1 {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "CSRF check failed"})
				return
			}
		}
		h(w, r)
	}
}
