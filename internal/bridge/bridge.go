// Package bridge wires a websocket to a tmux PTY client: binary frames carry
// raw PTY bytes, text frames carry JSON control messages (resize, presence,
// ping/pong). It scans PTY output for the terminal bell (BEL, 0x07) to raise
// attention events, and sends periodic heartbeats. All websocket writes are
// serialized through a single writer goroutine.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// PTY is the terminal endpoint the bridge pumps to/from (satisfied by
// *session.Client). Defined as an interface for testability.
type PTY interface {
	io.Reader
	io.Writer
	Resize(cols, rows uint16) error
}

const (
	heartbeatInterval = 15 * time.Second
	bellDebounce      = 3 * time.Second
	readBufSize       = 32 * 1024
)

type frame struct {
	typ  websocket.MessageType
	data []byte
}

// Bridge couples one websocket with one PTY client.
type Bridge struct {
	ws     *websocket.Conn
	client PTY
	out    chan frame

	// OnPresence is called with "foreground"/"background" presence updates.
	OnPresence func(state string)
	// OnAttention is called (debounced) when the PTY emits a bell.
	OnAttention func()
	// OnSwitch switches the tmux client to target; returns the resulting session
	// name (or "" on failure).
	OnSwitch func(target string) string
	// OnNew creates a session (kind "bash"|"remote", id for remotes) and switches
	// to it; returns the new session name (or "" on failure).
	OnNew func(kind, id string) string
	// Heartbeat overrides the ping interval (keeps idle connections alive
	// through reverse-proxy idle timeouts). Zero uses the default.
	Heartbeat time.Duration

	bmu      sync.Mutex
	lastBell time.Time
}

// New creates a bridge.
func New(ws *websocket.Conn, client PTY) *Bridge {
	ws.SetReadLimit(1 << 20)
	return &Bridge{ws: ws, client: client, out: make(chan frame, 256)}
}

// Send enqueues a text control frame to the client (used for in-page attention
// messages). Non-blocking: dropped if the buffer is full or the bridge is gone.
func (b *Bridge) Send(data []byte) {
	select {
	case b.out <- frame{websocket.MessageText, data}:
	default:
	}
}

// Run pumps data until either side closes or ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup

	// Single writer: serializes PTY output, heartbeats and control frames.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-b.out:
				wctx, c := context.WithTimeout(ctx, 10*time.Second)
				err := b.ws.Write(wctx, f.typ, f.data)
				c()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Heartbeat.
	wg.Add(1)
	go func() {
		defer wg.Done()
		hb := b.Heartbeat
		if hb <= 0 {
			hb = heartbeatInterval
		}
		t := time.NewTicker(hb)
		defer t.Stop()
		ping := []byte(`{"type":"ping"}`)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.Send(ping)
			}
		}
	}()

	// PTY -> websocket.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		buf := make([]byte, readBufSize)
		for {
			n, err := b.client.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				if bytes.IndexByte(data, 0x07) >= 0 {
					b.bell()
				}
				select {
				case b.out <- frame{websocket.MessageBinary, data}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// websocket -> PTY (this goroutine).
	b.readWS(ctx)
	cancel()
	wg.Wait()
}

func (b *Bridge) readWS(ctx context.Context) {
	for {
		typ, data, err := b.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if _, err := b.client.Write(data); err != nil {
				return
			}
			continue
		}
		var m struct {
			Type   string `json:"type"`
			Cols   uint16 `json:"cols"`
			Rows   uint16 `json:"rows"`
			State  string `json:"state"`
			Target string `json:"target"`
			Kind   string `json:"kind"`
			ID     string `json:"id"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "resize":
			if m.Cols > 0 && m.Rows > 0 {
				_ = b.client.Resize(m.Cols, m.Rows)
			}
		case "presence":
			if b.OnPresence != nil {
				b.OnPresence(m.State)
			}
		case "switch":
			if b.OnSwitch != nil {
				if name := b.OnSwitch(m.Target); name != "" {
					b.sendSession(name)
				}
			}
		case "new":
			if b.OnNew != nil {
				if name := b.OnNew(m.Kind, m.ID); name != "" {
					b.sendSession(name)
				}
			}
		case "pong", "ping":
			// liveness only
		}
	}
}

func (b *Bridge) sendSession(name string) {
	frame, _ := json.Marshal(map[string]string{"type": "session", "name": name})
	b.Send(frame)
}

func (b *Bridge) bell() {
	now := time.Now()
	b.bmu.Lock()
	if now.Sub(b.lastBell) < bellDebounce {
		b.bmu.Unlock()
		return
	}
	b.lastBell = now
	b.bmu.Unlock()
	if b.OnAttention != nil {
		b.OnAttention()
	}
}
