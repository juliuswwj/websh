package auth

import (
	"testing"
	"time"
)

func TestSessionStore(t *testing.T) {
	st := NewStore(time.Hour)
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
	st := NewStore(time.Millisecond)
	sid := st.New("bob", "1")
	time.Sleep(5 * time.Millisecond)
	if st.Get(sid) != nil {
		t.Fatal("expired session should be nil")
	}
}
