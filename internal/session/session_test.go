package session

import (
	"os/exec"
	"os/user"
	"strconv"
	"testing"
	"time"
)

func cleanEnv(home string) []string {
	return []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color"}
}

// TestSpawnPlumbing checks the PTY/tmux spawn path works (PTY allocated, resize
// and close succeed) regardless of the login shell.
func TestSpawnPlumbing(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(time.Hour, "http://127.0.0.1:0/internal/notify", func(string) string { return "tok" })
	spec := Spec{ID: "plumb"}
	c, err := m.Spawn(spec, u, 80, 24)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer killSession(&sessInfo{name: c.Session(), home: u.HomeDir})
	if err := c.Resize(100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLiveSessionsAndReap validates session listing, name parsing and idle
// reclamation against a shell-independent tmux session (runs `sleep`).
func TestLiveSessionsAndReap(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid64, _ := strconv.ParseUint(u.Uid, 10, 32)
	name := SessionName(uint32(uid64), "live")

	// Create a persistent session that does not depend on the login shell.
	create := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "120")
	create.Env = cleanEnv(u.HomeDir)
	create.Dir = u.HomeDir
	if out, err := create.CombinedOutput(); err != nil {
		t.Skipf("cannot create tmux session here: %v: %s", err, out)
	}
	defer killSession(&sessInfo{name: name, home: u.HomeDir})

	m := NewManager(time.Hour, "", nil)
	if !m.LiveSessions(u)["live"] {
		t.Fatal("LiveSessions did not find the live session")
	}

	// Register it and force idle reclamation.
	m.register(name, nil, u.HomeDir)
	m.idleTTL = 0
	m.reapIdle()
	if m.LiveSessions(u)["live"] {
		t.Fatal("session should have been reclaimed by the janitor")
	}
}

func TestListLabelRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid64, _ := strconv.ParseUint(u.Uid, 10, 32)
	name := SessionName(uint32(uid64), "labeltest")
	create := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "120")
	create.Env = cleanEnv(u.HomeDir)
	create.Dir = u.HomeDir
	if out, err := create.CombinedOutput(); err != nil {
		t.Skipf("cannot create tmux session: %v: %s", err, out)
	}
	defer killSession(&sessInfo{name: name, home: u.HomeDir})

	m := NewManager(time.Hour, "", nil)
	if err := m.SetLabel(u, "labeltest", "My Box | 1"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	found := false
	for _, li := range m.List(u) {
		if li.ID == "labeltest" {
			found = true
			if li.Label != "My Box | 1" {
				t.Fatalf("label = %q, want %q", li.Label, "My Box | 1")
			}
		}
	}
	if !found {
		t.Fatal("labeltest session not listed")
	}
}

func TestNextBashID(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(time.Hour, "", nil)
	id := m.NextBashID(u)
	if m.LiveSessions(u)[id] {
		t.Fatalf("NextBashID returned an in-use id %q", id)
	}
	if n, err := strconv.Atoi(id); err != nil || n < 1 {
		t.Fatalf("NextBashID should be a positive integer, got %q", id)
	}
}

func TestParseSessionName(t *testing.T) {
	uid, slug, ok := ParseSessionName("websh-1004-my-tab")
	if !ok || uid != "1004" || slug != "my-tab" {
		t.Fatalf("got uid=%q slug=%q ok=%v", uid, slug, ok)
	}
	if _, _, ok := ParseSessionName("other-session"); ok {
		t.Fatal("non-websh name should not parse")
	}
}

func TestBuildArgvSSH(t *testing.T) {
	spec := Spec{ID: "x", SSH: true, Host: "h.example", User: "deploy", Port: 2222, SSHOptions: []string{"-o", "ServerAliveInterval=30"}}
	argv := buildArgv(spec, "websh-1-x")
	want := []string{"tmux", "new-session", "-A", "-s", "websh-1-x", "ssh", "-tt", "-o", "ServerAliveInterval=30", "-p", "2222", "deploy@h.example"}
	if len(argv) != len(want) {
		t.Fatalf("argv len %d != %d: %v", len(argv), len(want), argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d]=%q want %q", i, argv[i], want[i])
		}
	}
}
