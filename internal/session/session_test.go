package session

import (
	"os/exec"
	"os/user"
	"strings"
	"testing"
	"time"
)

func cleanEnv(home string) []string {
	return []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "SHELL=/bin/bash"}
}

// mkSession creates a detached tmux session that doesn't depend on the login
// shell (runs `sleep`), and returns a cleanup func.
func mkSession(t *testing.T, u *user.User, name string) func() {
	t.Helper()
	c := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "300")
	c.Env = cleanEnv(u.HomeDir)
	c.Dir = u.HomeDir
	if out, err := c.CombinedOutput(); err != nil {
		t.Skipf("cannot create tmux session here: %v: %s", err, out)
	}
	return func() {
		k := exec.Command("tmux", "kill-session", "-t", name)
		k.Env = cleanEnv(u.HomeDir)
		_ = k.Run()
	}
}

func requireTmux(t *testing.T) *user.User {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestListAll proves websh lists ALL of the user's sessions, not just ones it
// created — the core fix.
func TestListAll(t *testing.T) {
	u := requireTmux(t)
	defer mkSession(t, u, "alpha")()
	defer mkSession(t, u, "beta")()

	m := NewManager()
	got := map[string]bool{}
	for _, li := range m.List(u) {
		got[li.Name] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Fatalf("List did not include both plain sessions: %v", got)
	}
}

// TestAttachSwitch attaches a top-level client and switches it between sessions.
func TestAttachSwitch(t *testing.T) {
	u := requireTmux(t)
	defer mkSession(t, u, "alpha")()
	defer mkSession(t, u, "beta")()

	m := NewManager()
	c, err := m.Attach(u, 80, 24)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer c.Close()
	go func() {
		b := make([]byte, 4096)
		for {
			if _, e := c.Read(b); e != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)

	if err := c.Switch("beta"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if !waitClientSession(u, c.tty, "beta") {
		t.Fatalf("client did not switch to beta (got %q)", clientSession(u, c.tty))
	}
	if err := c.Switch("alpha"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if !waitClientSession(u, c.tty, "alpha") {
		t.Fatalf("client did not switch to alpha")
	}
}

// TestNewBashAndKill creates a bash session, confirms the client lands on it,
// then kills it.
func TestNewBashAndKill(t *testing.T) {
	u := requireTmux(t)
	defer mkSession(t, u, "alpha")() // ensure a server + a fallback session

	m := NewManager()
	c, err := m.Attach(u, 80, 24)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer c.Close()
	go func() {
		b := make([]byte, 4096)
		for {
			if _, e := c.Read(b); e != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)

	name, err := c.NewBash()
	if err != nil {
		t.Fatalf("NewBash: %v", err)
	}
	if !waitClientSession(u, c.tty, name) {
		t.Fatalf("client did not switch to new bash %q", name)
	}
	if err := m.Kill(u, name); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	for _, li := range m.List(u) {
		if li.Name == name {
			t.Fatalf("session %q still present after Kill", name)
		}
	}
}

// TestRename renames a session.
func TestRename(t *testing.T) {
	u := requireTmux(t)
	cleanup := mkSession(t, u, "renA")
	defer func() {
		// the rename may have moved it; clean both names
		k := exec.Command("tmux", "kill-session", "-t", "renB")
		k.Env = cleanEnv(u.HomeDir)
		_ = k.Run()
		cleanup()
	}()

	m := NewManager()
	if err := m.Rename(u, "renA", "renB"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got := map[string]bool{}
	for _, li := range m.List(u) {
		got[li.Name] = true
	}
	if got["renA"] || !got["renB"] {
		t.Fatalf("rename not applied: %v", got)
	}
}

// TestAttachDeterministic guards the refresh bug: repeated attaches must all
// land on the SAME session, not hop across every session.
func TestAttachDeterministic(t *testing.T) {
	u := requireTmux(t)
	defer mkSession(t, u, "d1")()
	defer mkSession(t, u, "d2")()
	defer mkSession(t, u, "d3")()
	time.Sleep(200 * time.Millisecond)

	m := NewManager()
	var clients []*Client
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	first := ""
	for i := 0; i < 3; i++ {
		c, err := m.Attach(u, 80, 24)
		if err != nil {
			t.Fatalf("Attach %d: %v", i, err)
		}
		clients = append(clients, c)
		go func(c *Client) {
			b := make([]byte, 4096)
			for {
				if _, e := c.Read(b); e != nil {
					return
				}
			}
		}(c)
		time.Sleep(250 * time.Millisecond)
		got := c.CurrentSession()
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("attach %d landed on %q, want %q (sessions must not round-robin)", i, got, first)
		}
	}
}

// TestNextRemoteIndex checks remote sessions get distinct "<n>@<id>" names.
func TestNextRemoteIndex(t *testing.T) {
	u := requireTmux(t)
	m := NewManager()
	if got := m.nextRemoteIndex(u, "srv"); got != 0 {
		t.Fatalf("first index = %d, want 0", got)
	}
	defer mkSession(t, u, "0@srv")()
	if got := m.nextRemoteIndex(u, "srv"); got != 1 {
		t.Fatalf("after 0@srv, index = %d, want 1", got)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"main", "sh1", "GPU 01", "工作", "a_b-c", "0@srv"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "a.b", "a:b", "a|b", "x\ty", "@win"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true, want false", bad)
		}
	}
}

func TestSSHArgs(t *testing.T) {
	a := sshArgs(Spec{ID: "x", SSH: true, Host: "h.example", User: "deploy", Port: 2222, SSHOptions: []string{"-o", "ServerAliveInterval=30"}})
	want := []string{"ssh", "-tt", "-o", "ServerAliveInterval=30", "-p", "2222", "deploy@h.example"}
	if strings.Join(a, " ") != strings.Join(want, " ") {
		t.Fatalf("sshArgs = %v, want %v", a, want)
	}
}

// helpers ---------------------------------------------------------------------

func clientSession(u *user.User, tty string) string {
	c := exec.Command("tmux", "display-message", "-c", tty, "-p", "#{session_name}")
	c.Env = cleanEnv(u.HomeDir)
	out, _ := c.Output()
	return strings.TrimSpace(string(out))
}

func waitClientSession(u *user.User, tty, want string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if clientSession(u, tty) == want {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
