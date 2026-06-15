package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakePTY echoes whatever is written to it back to its reader and records
// resizes.
type fakePTY struct {
	r       *io.PipeReader
	w       *io.PipeWriter
	mu      sync.Mutex
	written []byte
	cols    uint16
	rows    uint16
	resized chan struct{}
}

func newFakePTY() *fakePTY {
	r, w := io.Pipe()
	return &fakePTY{r: r, w: w, resized: make(chan struct{}, 8)}
}

func (f *fakePTY) Read(p []byte) (int, error) { return f.r.Read(p) }

func (f *fakePTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.written = append(f.written, p...)
	f.mu.Unlock()
	go func() { _, _ = f.w.Write(p) }() // echo back to the websocket
	return len(p), nil
}

func (f *fakePTY) Resize(cols, rows uint16) error {
	f.mu.Lock()
	f.cols, f.rows = cols, rows
	f.mu.Unlock()
	select {
	case f.resized <- struct{}{}:
	default:
	}
	return nil
}

// TestBridgeSwitch verifies a {type:"switch"} control frame invokes OnSwitch
// and the bridge replies with the resulting session name.
func TestBridgeSwitch(t *testing.T) {
	fp := newFakePTY()
	gotTarget := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		b := New(c, fp)
		b.OnSwitch = func(target string) string { gotTarget <- target; return target }
		b.Run(r.Context())
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"switch","target":"work"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-gotTarget:
		if got != "work" {
			t.Fatalf("OnSwitch target = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnSwitch not called")
	}
	// Expect a {type:"session","name":"work"} reply.
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"work"`) {
			return
		}
	}
}

// TestBridgeInputAndResize verifies that binary frames reach the PTY as input
// and that a JSON resize control frame triggers a PTY resize.
func TestBridgeInputAndResize(t *testing.T) {
	fp := newFakePTY()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		New(c, fp).Run(r.Context())
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Resize control frame (text JSON).
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	// Terminal input (binary).
	if err := c.Write(ctx, websocket.MessageBinary, []byte("echo hi\r")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// The input must be echoed back through the bridge.
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if typ != websocket.MessageBinary || string(data) != "echo hi\r" {
		t.Fatalf("echo: typ=%v data=%q", typ, data)
	}

	// Resize must have reached the PTY.
	select {
	case <-fp.resized:
	case <-time.After(2 * time.Second):
		t.Fatal("resize never reached the PTY")
	}
	fp.mu.Lock()
	cols, rows := fp.cols, fp.rows
	written := string(fp.written)
	fp.mu.Unlock()
	if cols != 120 || rows != 40 {
		t.Fatalf("resize dims cols=%d rows=%d, want 120x40", cols, rows)
	}
	if written != "echo hi\r" {
		t.Fatalf("PTY received %q, want %q", written, "echo hi\r")
	}
}
