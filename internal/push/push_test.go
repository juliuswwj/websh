package push

import (
	"path/filepath"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func TestSubscribeDedupAndPersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "subs.json")
	s := NewStore(p, "priv", "pub", "mailto:a@b")
	if s.PublicKey() != "pub" {
		t.Fatal("PublicKey")
	}

	sub := webpush.Subscription{Endpoint: "https://push/1"}
	s.Subscribe("u1", sub)
	s.Subscribe("u1", sub) // duplicate endpoint, ignored
	s.Subscribe("u1", webpush.Subscription{Endpoint: "https://push/2"})

	// Reload from disk and confirm dedup + persistence.
	s2 := NewStore(p, "priv", "pub", "mailto:a@b")
	if got := len(s2.subs["u1"]); got != 2 {
		t.Fatalf("want 2 persisted subscriptions, got %d", got)
	}
}
