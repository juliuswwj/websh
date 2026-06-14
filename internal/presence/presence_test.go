package presence

import "testing"

func TestNotifyDecision(t *testing.T) {
	tr := New()

	// No connection at all -> push.
	if !tr.Notify("u", "t", []byte("x")) {
		t.Fatal("no connection should push")
	}

	got := make(chan []byte, 1)
	c := tr.Add("u", "t", func(b []byte) { got <- b })

	// New connections default to foreground -> suppress push, deliver in-page.
	if tr.Notify("u", "t", []byte("hi")) {
		t.Fatal("foreground should suppress push")
	}
	select {
	case b := <-got:
		if string(b) != "hi" {
			t.Fatalf("delivered frame %q", b)
		}
	default:
		t.Fatal("in-page frame not delivered to foreground conn")
	}

	// Backgrounded -> push.
	tr.SetState(c, "background")
	if !tr.Notify("u", "t", []byte("hi")) {
		t.Fatal("background should push")
	}

	// A different tab is unaffected.
	if !tr.Notify("u", "other", []byte("hi")) {
		t.Fatal("unknown tab should push")
	}

	// Removed -> push.
	tr.Remove(c)
	if !tr.Notify("u", "t", []byte("hi")) {
		t.Fatal("after Remove should push")
	}
}
