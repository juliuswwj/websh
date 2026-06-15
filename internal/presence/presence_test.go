package presence

import "testing"

func TestNotifyDecision(t *testing.T) {
	tr := New()

	// No connection -> push.
	if !tr.Notify("u", []byte("x")) {
		t.Fatal("no connection should push")
	}

	got := make(chan []byte, 1)
	c := tr.Add("u", func(b []byte) { got <- b })

	// New connections default to foreground -> suppress push, deliver in-page.
	if tr.Notify("u", []byte("hi")) {
		t.Fatal("foreground should suppress push")
	}
	select {
	case b := <-got:
		if string(b) != "hi" {
			t.Fatalf("delivered %q", b)
		}
	default:
		t.Fatal("in-page frame not delivered")
	}

	// Backgrounded -> push.
	tr.SetState(c, "background")
	if !tr.Notify("u", []byte("hi")) {
		t.Fatal("background should push")
	}

	// Another user is unaffected.
	if !tr.Notify("other", []byte("hi")) {
		t.Fatal("unknown user should push")
	}

	// Removed -> push.
	tr.Remove(c)
	if !tr.Notify("u", []byte("hi")) {
		t.Fatal("after Remove should push")
	}
}
