package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// CookieName is the web-login session cookie.
const CookieName = "websh_sid"

// Session is a logged-in web session.
type Session struct {
	User string
	UID  string // numeric uid as string (passwd Uid)
	Exp  time.Time
}

// Store is a web-session store with a fixed TTL (7 days), optionally persisted
// to a JSON file so sessions survive a daemon restart.
type Store struct {
	mu   sync.Mutex
	m    map[string]*Session
	ttl  time.Duration
	path string
}

// NewStore creates a session store with the given TTL. If path is non-empty the
// store is loaded from and persisted to that file (live, non-expired sessions
// only), so restarting the daemon does not force everyone to log in again.
func NewStore(ttl time.Duration, path string) *Store {
	s := &Store{m: make(map[string]*Session), ttl: ttl, path: path}
	if path != "" {
		s.load()
	}
	return s
}

// load reads persisted sessions, dropping any that have expired.
func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]*Session
	if json.Unmarshal(data, &m) != nil {
		return
	}
	now := time.Now()
	for sid, sess := range m {
		if sess != nil && now.Before(sess.Exp) {
			s.m[sid] = sess
		}
	}
}

// saveLocked atomically writes the session map. The caller holds s.mu.
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.m)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// TTL returns the configured session lifetime.
func (s *Store) TTL() time.Duration { return s.ttl }

// New mints a session and returns its id.
func (s *Store) New(user, uid string) string {
	sid := randToken(32)
	s.mu.Lock()
	s.m[sid] = &Session{User: user, UID: uid, Exp: time.Now().Add(s.ttl)}
	s.saveLocked()
	s.mu.Unlock()
	return sid
}

// Get returns a live (non-expired) session or nil.
func (s *Store) Get(sid string) *Session {
	if sid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[sid]
	if sess == nil {
		return nil
	}
	if time.Now().After(sess.Exp) {
		delete(s.m, sid)
		return nil
	}
	return sess
}

// Delete drops a session.
func (s *Store) Delete(sid string) {
	s.mu.Lock()
	delete(s.m, sid)
	s.saveLocked()
	s.mu.Unlock()
}

// GC loop deletes expired sessions until the context channel closes.
func (s *Store) GC(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := time.Now()
			s.mu.Lock()
			n := len(s.m)
			for sid, sess := range s.m {
				if now.After(sess.Exp) {
					delete(s.m, sid)
				}
			}
			if len(s.m) != n {
				s.saveLocked()
			}
			s.mu.Unlock()
		}
	}
}

// FromRequest resolves the session from the request cookie.
func (s *Store) FromRequest(r *http.Request) *Session {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	return s.Get(c.Value)
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
