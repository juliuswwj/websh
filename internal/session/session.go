// Package session attaches a single top-level tmux client per websocket and
// drives it (switch/new/rename/kill) so the user sees and continues ALL of
// their tmux sessions — not just ones websh created.
//
// One websocket = one PTY = one tmux client (identified by its slave tty, used
// as the switch-client target). Sessions are the user's own and are never
// auto-reclaimed.
package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

var (
	errNeedRoot = errors.New("websh must run as root to spawn sessions for other users")
	errBadName  = errors.New("invalid tmux session name")
)

// Spec describes a session to create: a local shell, or an SSH connection.
type Spec struct {
	ID         string // tmux session name to use
	SSH        bool
	Host       string
	User       string
	Port       int
	SSHOptions []string
}

// Manager runs tmux commands on behalf of users.
type Manager struct{}

// NewManager creates a session manager.
func NewManager() *Manager { return &Manager{} }

// ValidName reports whether s is a usable tmux session name. tmux uses ':' and
// '.' in target syntax, and we use '|' as a list separator, so those (and
// control chars) are rejected.
func ValidName(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '@' { // leading '@' is a tmux window-id
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == ':' || r == '.' || r == '|' {
			return false
		}
	}
	return true
}

// Client is one PTY-attached tmux client (one websocket).
type Client struct {
	mgr  *Manager
	user *user.User
	ptmx *os.File
	cmd  *exec.Cmd
	tty  string // slave tty path == tmux client_tty, used for switch-client -c
}

// Attach starts a tmux client for user u: attach to the most-recent session if
// any exist, else create a default one. cols/rows seed the window size.
func (m *Manager) Attach(u *user.User, cols, rows uint16) (*Client, error) {
	uid, gid, groups, err := credentials(u)
	if err != nil {
		return nil, err
	}
	var cred *syscall.Credential
	if os.Geteuid() == 0 {
		cred = &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}
	} else if uid != uint32(os.Getuid()) {
		return nil, errNeedRoot
	}

	// Attach to a SPECIFIC session: `attach-session` with no -t lands on the most
	// recent UNATTACHED session, so repeated attaches (refresh/reconnect churn)
	// hop across every session. Picking an explicit target makes it deterministic
	// — always the most-recently-active session, i.e. where you left off.
	var argv []string
	if target := m.mostRecent(u); target != "" {
		argv = []string{"tmux", "attach-session", "-t", target}
	} else {
		argv = []string{"tmux", "new-session", "-s", "main"}
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = u.HomeDir
	cmd.Env = baseEnv(u)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Credential: cred}
	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, fmt.Errorf("attach tmux: %w", err)
	}
	clientTTY := tty.Name()
	_ = tty.Close() // parent keeps only the master

	// Best-effort: surface bells from any session to the attached client.
	go func() { _ = m.tmuxAsUser(u, "set-option", "-g", "bell-action", "any").Run() }()

	return &Client{mgr: m, user: u, ptmx: ptmx, cmd: cmd, tty: clientTTY}, nil
}

// Read/Write/Resize/Close operate on the PTY.
func (c *Client) Read(p []byte) (int, error)  { return c.ptmx.Read(p) }
func (c *Client) Write(p []byte) (int, error) { return c.ptmx.Write(p) }

// Resize updates the PTY size; TIOCSWINSZ alone doesn't reliably signal the
// tmux client, so send SIGWINCH explicitly.
func (c *Client) Resize(cols, rows uint16) error {
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		return err
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGWINCH)
	}
	return nil
}

// Close tears down this client; the tmux sessions live on.
func (c *Client) Close() error {
	_ = c.ptmx.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGHUP)
	}
	_ = c.cmd.Wait()
	return nil
}

// Switch points this client at an existing session.
func (c *Client) Switch(target string) error {
	if !ValidName(target) {
		return errBadName
	}
	return c.mgr.tmuxAsUser(c.user, "switch-client", "-c", c.tty, "-t", target).Run()
}

// NewBash creates a fresh local shell session and switches to it.
func (c *Client) NewBash() (string, error) {
	name := c.mgr.nextName(c.user)
	if err := c.mgr.tmuxAsUser(c.user, "new-session", "-d", "-s", name).Run(); err != nil {
		return "", err
	}
	return name, c.Switch(name)
}

// NewRemote opens another tmux session ON the remote and switches to it. It is
// backed by a local proxy session named "<n>@<id>" whose command sshes to the
// host and runs `tmux new-session -A -s <n>` there — so "0@server" is remote
// tmux session "0". The remote sessions persist independently of websh.
func (c *Client) NewRemote(spec Spec) (string, error) {
	if !ValidName(spec.ID) {
		return "", errBadName
	}
	idx := strconv.Itoa(c.mgr.nextRemoteIndex(c.user, spec.ID))
	name := idx + "@" + spec.ID // local proxy == display name
	argv := []string{"new-session", "-d", "-s", name}
	argv = append(argv, sshArgs(spec)...)
	argv = append(argv, "tmux", "new-session", "-A", "-s", idx) // remote session name
	if err := c.mgr.tmuxAsUser(c.user, argv...).Run(); err != nil {
		return "", err
	}
	return name, c.Switch(name)
}

// nextRemoteIndex returns the smallest free index n such that "<n>@<id>" is not
// already a local (proxy) session.
func (m *Manager) nextRemoteIndex(u *user.User, id string) int {
	suffix := "@" + id
	used := map[int]bool{}
	for _, li := range m.List(u) {
		if rest, ok := strings.CutSuffix(li.Name, suffix); ok {
			if n, err := strconv.Atoi(rest); err == nil {
				used[n] = true
			}
		}
	}
	for i := 0; ; i++ {
		if !used[i] {
			return i
		}
	}
}

// CurrentSession returns the name of the session this client is attached to.
func (c *Client) CurrentSession() string {
	out, err := c.mgr.tmuxAsUser(c.user, "display-message", "-c", c.tty, "-p", "#{session_name}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// mostRecent returns the most-recently-attached session name (the one to land
// on when (re)attaching), or "" if there are none.
func (m *Manager) mostRecent(u *user.User) string {
	out, err := m.tmuxAsUser(u, "list-sessions", "-F", "#{session_last_attached}|#{session_name}").Output()
	if err != nil {
		return ""
	}
	best, bestTs := "", int64(-1)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		ts, name, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		v, _ := strconv.ParseInt(ts, 10, 64)
		if v > bestTs {
			bestTs, best = v, name
		}
	}
	return best
}

// LiveInfo describes one tmux session.
type LiveInfo struct {
	Name     string
	Attached bool
	Window   string
}

// List returns ALL tmux sessions for user u.
func (m *Manager) List(u *user.User) []LiveInfo {
	// tmux drops literal tabs in -F output; use '|'. Session names with '|' are
	// rejected by ValidName, and the count never contains '|'.
	out, err := m.tmuxAsUser(u, "list-sessions", "-F", "#{session_name}|#{session_attached}|#{window_name}").Output()
	if err != nil {
		return nil // no server / no sessions
	}
	var res []LiveInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		li := LiveInfo{Name: parts[0]}
		if len(parts) > 1 {
			li.Attached = parts[1] != "" && parts[1] != "0"
		}
		if len(parts) > 2 {
			li.Window = parts[2]
		}
		res = append(res, li)
	}
	return res
}

// Rename renames a session.
func (m *Manager) Rename(u *user.User, old, name string) error {
	if !ValidName(old) || !ValidName(name) {
		return errBadName
	}
	return m.tmuxAsUser(u, "rename-session", "-t", old, name).Run()
}

// Kill terminates a session.
func (m *Manager) Kill(u *user.User, name string) error {
	if !ValidName(name) {
		return errBadName
	}
	return m.tmuxAsUser(u, "kill-session", "-t", name).Run()
}

// nextName returns the smallest unused "sh<N>" name.
func (m *Manager) nextName(u *user.User) string {
	used := map[string]bool{}
	for _, li := range m.List(u) {
		used[li.Name] = true
	}
	for i := 1; ; i++ {
		n := "sh" + strconv.Itoa(i)
		if !used[n] {
			return n
		}
	}
}

// RenameRemote renames a session ON the remote (the prefix of "<n>@<id>").
func (m *Manager) RenameRemote(u *user.User, spec Spec, old, name string) error {
	if !ValidName(old) || !ValidName(name) {
		return errBadName
	}
	argv := sshExecArgs(spec, "tmux", "rename-session", "-t", old, name)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = u.HomeDir
	cmd.Env = []string{"HOME=" + u.HomeDir, "PATH=/usr/local/bin:/usr/bin:/bin"}
	if os.Geteuid() == 0 {
		if uid, gid, groups, err := credentials(u); err == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}
		}
	}
	return cmd.Run()
}

// sshExecArgs builds a non-interactive ssh command (key auth only) running
// remoteCmd on the remote host.
func sshExecArgs(spec Spec, remoteCmd ...string) []string {
	ssh := []string{"ssh", "-o", "BatchMode=yes"}
	ssh = append(ssh, spec.SSHOptions...)
	if spec.Port != 0 {
		ssh = append(ssh, "-p", strconv.Itoa(spec.Port))
	}
	target := spec.Host
	if spec.User != "" {
		target = spec.User + "@" + spec.Host
	}
	ssh = append(ssh, target)
	return append(ssh, remoteCmd...)
}

func sshArgs(spec Spec) []string {
	ssh := []string{"ssh", "-tt"}
	ssh = append(ssh, spec.SSHOptions...)
	if spec.Port != 0 {
		ssh = append(ssh, "-p", strconv.Itoa(spec.Port))
	}
	target := spec.Host
	if spec.User != "" {
		target = spec.User + "@" + spec.Host
	}
	return append(ssh, target)
}

func baseEnv(u *user.User) []string {
	return []string{
		"HOME=" + u.HomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
	}
}

// tmuxAsUser builds a tmux command running as user u (privileges dropped when root).
func (m *Manager) tmuxAsUser(u *user.User, args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", args...)
	cmd.Dir = u.HomeDir
	cmd.Env = []string{"HOME=" + u.HomeDir, "PATH=/usr/local/bin:/usr/bin:/bin"}
	if os.Geteuid() == 0 {
		if uid, gid, groups, err := credentials(u); err == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}
		}
	}
	return cmd
}

func credentials(u *user.User) (uid, gid uint32, groups []uint32, err error) {
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("bad uid: %w", err)
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("bad gid: %w", err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return 0, 0, nil, fmt.Errorf("group ids: %w", err)
	}
	for _, g := range gids {
		if v, e := strconv.ParseUint(g, 10, 32); e == nil {
			groups = append(groups, uint32(v))
		}
	}
	return uint32(uid64), uint32(gid64), groups, nil
}
