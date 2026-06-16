package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/creack/pty"
)

// TestSetRawTTY verifies the proxy clears the termios bits that cause the two
// reported xwb bugs: ECHO (double echo) and ICRNL (Enter -> newline instead of
// submit in claude/TUIs), plus the rest of raw mode.
func TestSetRawTTY(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	fd := int(tty.Fd())

	old, err := setRawTTY(fd)
	if err != nil {
		t.Fatalf("setRawTTY: %v", err)
	}
	var cur syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Lflag&syscall.ECHO != 0 {
		t.Error("ECHO not cleared -> keystrokes would echo twice")
	}
	if cur.Lflag&syscall.ICANON != 0 {
		t.Error("ICANON not cleared -> input would be line-buffered")
	}
	if cur.Iflag&syscall.ICRNL != 0 {
		t.Error("ICRNL not cleared -> Enter (CR) would arrive as newline")
	}
	if cur.Oflag&syscall.OPOST != 0 {
		t.Error("OPOST not cleared -> output would be post-processed")
	}

	// Restoring brings the cooked bits back.
	if err := ioctlTermios(fd, syscall.TCSETS, old); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var restored syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Lflag&syscall.ECHO == 0 {
		t.Error("restore failed: ECHO still off")
	}
}

func TestSetRawTTYNonTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := setRawTTY(int(r.Fd())); err == nil {
		t.Fatal("expected an error setting raw mode on a non-tty")
	}
}
