package session

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestResizePropagatesToTmux verifies that Client.Resize actually moves the tmux
// window (Setsize alone does not — it needs the explicit SIGWINCH).
func TestResizePropagatesToTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	home, _ := os.UserHomeDir()
	name := "websh-test-resize"
	env := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "SHELL=/bin/bash"}
	tmux := func(args ...string) string {
		c := exec.Command("tmux", args...)
		c.Env = env
		out, _ := c.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	tmux("kill-session", "-t", name)

	cmd := exec.Command("tmux", "new-session", "-A", "-s", name)
	cmd.Env = env
	cmd.Dir = home
	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: 24, Cols: 80}, &syscall.SysProcAttr{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = ptmx.Close(); tmux("kill-session", "-t", name) }()
	go func() {
		b := make([]byte, 4096)
		for {
			if _, e := ptmx.Read(b); e != nil {
				return
			}
		}
	}()

	c := &Client{mgr: NewManager(time.Hour, "", nil), name: name, ptmx: ptmx, cmd: cmd}
	time.Sleep(300 * time.Millisecond)

	if err := c.Resize(132, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if tmux("display-message", "-p", "-t", name, "#{window_width}") == "132" {
			return // success
		}
		if time.Now().After(deadline) {
			t.Fatalf("tmux window width did not follow Resize: got %q", tmux("display-message", "-p", "-t", name, "#{window_width}"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
