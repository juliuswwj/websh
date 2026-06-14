package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestLoggedMiddlewareAllowsWebSocket guards against the logging wrapper hiding
// the http.Hijacker, which would break every websocket upgrade.
func TestLoggedMiddlewareAllowsWebSocket(t *testing.T) {
	s := &server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = c.Write(r.Context(), websocket.MessageText, []byte("hi"))
	})

	srv := httptest.NewServer(s.logged(mux))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("websocket dial through logged middleware failed: %v", err)
	}
	defer c.CloseNow()

	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != "hi" {
		t.Fatalf("read: typ=%v data=%q err=%v", typ, data, err)
	}
}
