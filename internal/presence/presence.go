// Package presence tracks whether a user's terminal tabs are foregrounded in a
// live PWA, and decides whether an attention event should be a push or an
// in-page message.
//
// Rule: a push is sent unless a live, foregrounded websocket exists for the
// (uid, tab). If one does, the attention is delivered in-page instead and the
// push is suppressed. No live websocket (browser closed / heartbeat timed out)
// always pushes.
package presence

import "sync"

// Conn is a live websocket presence handle for one (uid, tab).
type Conn struct {
	uid   string
	tab   string
	state string // "foreground" | "background"
	send  func([]byte)
}

// Tracker holds presence for all connections.
type Tracker struct {
	mu    sync.Mutex
	conns map[string]map[string]map[*Conn]struct{} // uid -> tab -> set
}

// New creates a presence tracker.
func New() *Tracker {
	return &Tracker{conns: make(map[string]map[string]map[*Conn]struct{})}
}

// Add registers a live connection. send delivers a text control frame to that
// client (serialized via the client's writer). The returned Conn is used for
// SetState and Remove.
func (t *Tracker) Add(uid, tab string, send func([]byte)) *Conn {
	c := &Conn{uid: uid, tab: tab, state: "foreground", send: send}
	t.mu.Lock()
	if t.conns[uid] == nil {
		t.conns[uid] = make(map[string]map[*Conn]struct{})
	}
	if t.conns[uid][tab] == nil {
		t.conns[uid][tab] = make(map[*Conn]struct{})
	}
	t.conns[uid][tab][c] = struct{}{}
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
	if tabs := t.conns[c.uid]; tabs != nil {
		if set := tabs[c.tab]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(tabs, c.tab)
			}
		}
		if len(tabs) == 0 {
			delete(t.conns, c.uid)
		}
	}
	t.mu.Unlock()
}

// Notify delivers an attention event for (uid, tab). If a foregrounded
// connection exists, it receives the in-page frame and the function returns
// false (suppress push). Otherwise it returns true (caller should push).
func (t *Tracker) Notify(uid, tab string, frame []byte) (shouldPush bool) {
	t.mu.Lock()
	var fg []func([]byte)
	for c := range t.conns[uid][tab] {
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
