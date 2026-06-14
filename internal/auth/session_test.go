package auth

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSessionStore(t *testing.T) {
	st := NewStore(time.Hour, "")
	sid := st.New("alice", "1000")
	s := st.Get(sid)
	if s == nil || s.User != "alice" || s.UID != "1000" {
		t.Fatalf("Get returned %+v", s)
	}
	st.Delete(sid)
	if st.Get(sid) != nil {
		t.Fatal("session should be gone after Delete")
	}
	if st.Get("nonexistent") != nil {
		t.Fatal("unknown sid should be nil")
	}
}

func TestSessionExpiry(t *testing.T) {
	st := NewStore(time.Millisecond, "")
	sid := st.New("bob", "1")
	time.Sleep(5 * time.Millisecond)
	if st.Get(sid) != nil {
		t.Fatal("expired session should be nil")
	}
}

// TestSessionPersistence simulates a daemon restart: a session minted by one
// store must be valid in a new store loading the same file.
func TestSessionPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	st1 := NewStore(time.Hour, path)
	sid := st1.New("carol", "1001")

	st2 := NewStore(time.Hour, path) // "restart"
	s := st2.Get(sid)
	if s == nil || s.User != "carol" || s.UID != "1001" {
		t.Fatalf("session not restored after restart: %+v", s)
	}

	// Logout must persist too.
	st2.Delete(sid)
	st3 := NewStore(time.Hour, path)
	if st3.Get(sid) != nil {
		t.Fatal("deleted session should not come back after restart")
	}
}

// TestSessionPersistenceDropsExpired ensures expired sessions are not reloaded.
func TestSessionPersistenceDropsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	st1 := NewStore(time.Millisecond, path)
	sid := st1.New("dave", "1")
	time.Sleep(5 * time.Millisecond)

	st2 := NewStore(time.Hour, path)
	if st2.Get(sid) != nil {
		t.Fatal("expired session should not be reloaded")
	}
}
