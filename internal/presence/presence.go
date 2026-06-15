// Package presence tracks whether a user has a live, foregrounded PWA and
// decides whether an attention event should be a push or an in-page message.
//
// Rule: push unless a live foregrounded connection exists for the user; if one
// does, deliver the attention in-page and suppress the push. No live connection
// (browser closed / heartbeat timed out) always pushes.
package presence

import "sync"

// Conn is a live websocket presence handle for one user.
type Conn struct {
	uid   string
	state string // "foreground" | "background"
	send  func([]byte)
}

// Tracker holds presence for all connections.
type Tracker struct {
	mu    sync.Mutex
	conns map[string]map[*Conn]struct{} // uid -> set
}

// New creates a presence tracker.
func New() *Tracker {
	return &Tracker{conns: make(map[string]map[*Conn]struct{})}
}

// Add registers a live connection. send delivers a text control frame to that
// client (serialized via the client's writer).
func (t *Tracker) Add(uid string, send func([]byte)) *Conn {
	c := &Conn{uid: uid, state: "foreground", send: send}
	t.mu.Lock()
	if t.conns[uid] == nil {
		t.conns[uid] = make(map[*Conn]struct{})
	}
	t.conns[uid][c] = struct{}{}
	t.mu.Unlock()
	return c
}

// SetState updates a connection's foreground/background state.
func (t *Tracker) SetState(c *Conn, state string) {
	if c == nil {
		return
	}
	t.mu.Lock()
	c.state = state
	t.mu.Unlock()
}

// Remove drops a connection on disconnect.
func (t *Tracker) Remove(c *Conn) {
	if c == nil {
		return
	}
	t.mu.Lock()
	if set := t.conns[c.uid]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(t.conns, c.uid)
		}
	}
	t.mu.Unlock()
}

// Notify delivers an attention event for a user. If a foregrounded connection
// exists, it receives the in-page frame and the function returns false (suppress
// push). Otherwise it returns true (caller should push).
func (t *Tracker) Notify(uid string, frame []byte) (shouldPush bool) {
	t.mu.Lock()
	var fg []func([]byte)
	for c := range t.conns[uid] {
		if c.state == "foreground" {
			fg = append(fg, c.send)
		}
	}
	t.mu.Unlock()

	if len(fg) == 0 {
		return true
	}
	for _, send := range fg {
		send(frame)
	}
	return false
}
