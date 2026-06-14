// Package session spawns and tracks tmux-backed terminal sessions.
//
// Each terminal maps to a persistent, per-user tmux session named
// "websh-<uid>-<slug>". A websocket connection spawns a short-lived PTY running
// `tmux new-session -A -s <name>` (attach-or-create) as the target user. When
// the websocket drops, that PTY/attach client dies but the tmux session lives
// on, so reconnecting reattaches with the screen intact — this is the fix for
// the reference project's fragile-websocket problem.
//
// A background janitor reclaims tmux sessions with no user input for idleTTL
// (3 days) via `tmux kill-session`.
package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

var errNeedRoot = errors.New("websh must run as root to spawn sessions for other users")

// Spec describes what to run in a session: a local shell, or an SSH connection.
type Spec struct {
	ID         string // session id (tmux slug)
	SSH        bool   // false = local shell, true = ssh to Host
	Host       string
	User       string
	Port       int
	SSHOptions []string
}

// Manager owns the live tmux-session registry and idle reclamation.
type Manager struct {
	mu        sync.Mutex
	sessions  map[string]*sessInfo // key: tmux session name
	idleTTL   time.Duration
	notifyURL string
	tokenFn   func(sessionName string) string
}

type sessInfo struct {
	name      string
	cred      *syscall.Credential // nil when not dropping privileges
	home      string
	lastInput time.Time
}

// NewManager creates a session manager. notifyURL/tokenFn are injected into the
// shell environment so websh-notify can reach the daemon (token is stateless,
// derived per session name, so it survives daemon restarts).
func NewManager(idleTTL time.Duration, notifyURL string, tokenFn func(string) string) *Manager {
	return &Manager{
		sessions:  make(map[string]*sessInfo),
		idleTTL:   idleTTL,
		notifyURL: notifyURL,
		tokenFn:   tokenFn,
	}
}

// SessionName returns the deterministic tmux session name for a uid + config id.
func SessionName(uid uint32, id string) string {
	return fmt.Sprintf("websh-%d-%s", uid, id)
}

// ParseSessionName extracts the uid and slug from a tmux session name.
func ParseSessionName(name string) (uid string, slug string, ok bool) {
	rest, found := strings.CutPrefix(name, "websh-")
	if !found {
		return "", "", false
	}
	u, s, found := strings.Cut(rest, "-")
	if !found || u == "" {
		return "", "", false
	}
	return u, s, true
}

// Client is one PTY/attach connection to a tmux session.
type Client struct {
	mgr  *Manager
	name string
	ptmx *os.File
	cmd  *exec.Cmd
}

// Spawn attaches (or creates) the tmux session for spec as user u and returns a
// PTY client. cols/rows seed the initial window size.
func (m *Manager) Spawn(spec Spec, u *user.User, cols, rows uint16) (*Client, error) {
	uid, gid, groups, err := credentials(u)
	if err != nil {
		return nil, err
	}
	name := SessionName(uid, spec.ID)
	argv := buildArgv(spec, name)

	attrs := &syscall.SysProcAttr{}
	var cred *syscall.Credential
	if os.Geteuid() == 0 {
		cred = &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}
		attrs.Credential = cred
	} else if uid != uint32(os.Getuid()) {
		return nil, errNeedRoot
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = u.HomeDir
	cmd.Env = m.buildEnv(u, spec.ID, name)

	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: rows, Cols: cols}, attrs)
	if err != nil {
		return nil, fmt.Errorf("spawn tmux: %w", err)
	}

	m.register(name, cred, u.HomeDir)
	return &Client{mgr: m, name: name, ptmx: ptmx, cmd: cmd}, nil
}

// Read returns terminal output bytes.
func (c *Client) Read(p []byte) (int, error) { return c.ptmx.Read(p) }

// Write forwards user input to the PTY and marks the session active (only real
// user input refreshes the idle timer — output, resize and heartbeats do not).
func (c *Client) Write(p []byte) (int, error) {
	c.mgr.touch(c.name)
	return c.ptmx.Write(p)
}

// Resize updates the PTY window size. Setting TIOCSWINSZ on the master does not
// reliably deliver SIGWINCH to the attached tmux client on its own, so we signal
// the client explicitly — without it tmux keeps the stale window size.
func (c *Client) Resize(cols, rows uint16) error {
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		return err
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGWINCH)
	}
	return nil
}

// Session returns the tmux session name.
func (c *Client) Session() string { return c.name }

// Close tears down this attach client without killing the persistent tmux session.
func (c *Client) Close() error {
	_ = c.ptmx.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGHUP)
	}
	_ = c.cmd.Wait()
	return nil
}

func (m *Manager) register(name string, cred *syscall.Credential, home string) {
	m.mu.Lock()
	si := m.sessions[name]
	if si == nil {
		si = &sessInfo{name: name, cred: cred, home: home}
		m.sessions[name] = si
	}
	si.lastInput = time.Now()
	m.mu.Unlock()
}

func (m *Manager) touch(name string) {
	m.mu.Lock()
	if si := m.sessions[name]; si != nil {
		si.lastInput = time.Now()
	}
	m.mu.Unlock()
}

func (m *Manager) buildEnv(u *user.User, id, name string) []string {
	env := []string{
		"HOME=" + u.HomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"WEBSH_SESSION=" + name,
		"WEBSH_TAB_ID=" + id,
		"WEBSH_NOTIFY_URL=" + m.notifyURL,
	}
	if m.tokenFn != nil {
		env = append(env, "WEBSH_NOTIFY_TOKEN="+m.tokenFn(name))
	}
	return env
}

// Janitor reclaims idle tmux sessions until stop is closed.
func (m *Manager) Janitor(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	now := time.Now()
	m.mu.Lock()
	var dead []*sessInfo
	for name, si := range m.sessions {
		if now.Sub(si.lastInput) > m.idleTTL {
			dead = append(dead, si)
			delete(m.sessions, name)
		}
	}
	m.mu.Unlock()
	for _, si := range dead {
		killSession(si)
	}
}

func killSession(si *sessInfo) {
	cmd := exec.Command("tmux", "kill-session", "-t", si.name)
	cmd.Dir = si.home
	cmd.Env = []string{"HOME=" + si.home, "PATH=/usr/local/bin:/usr/bin:/bin"}
	if si.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: si.cred}
	}
	_ = cmd.Run()
}

func buildArgv(spec Spec, name string) []string {
	base := []string{"tmux", "new-session", "-A", "-s", name}
	if !spec.SSH {
		return base
	}
	ssh := []string{"ssh", "-tt"}
	ssh = append(ssh, spec.SSHOptions...)
	if spec.Port != 0 {
		ssh = append(ssh, "-p", strconv.Itoa(spec.Port))
	}
	target := spec.Host
	if spec.User != "" {
		target = spec.User + "@" + spec.Host
	}
	ssh = append(ssh, target)
	return append(base, ssh...)
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

// tmuxAsUser builds a tmux command that runs as user u (privileges dropped when
// the daemon is root).
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

// LiveInfo describes one live websh tmux session.
type LiveInfo struct {
	ID       string
	Label    string // user-set display name (@websh_label), may be empty
	Attached bool
}

// List returns all live websh tmux sessions for user u.
func (m *Manager) List(u *user.User) []LiveInfo {
	uid, _, _, err := credentials(u)
	if err != nil {
		return nil
	}
	// tmux drops literal tabs in -F output, so use '|' as the separator. The
	// label goes last (SplitN keeps the remainder) so a '|' inside a label is safe;
	// session names and the attached count never contain '|'.
	out, err := m.tmuxAsUser(u, "list-sessions", "-F", "#{session_name}|#{session_attached}|#{@websh_label}").Output()
	if err != nil {
		return nil // no server / no sessions
	}
	want := strconv.FormatUint(uint64(uid), 10)
	var res []LiveInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		su, slug, ok := ParseSessionName(parts[0])
		if !ok || su != want {
			continue
		}
		li := LiveInfo{ID: slug}
		if len(parts) > 1 {
			li.Attached = parts[1] != "" && parts[1] != "0"
		}
		if len(parts) > 2 {
			li.Label = parts[2]
		}
		res = append(res, li)
	}
	return res
}

// LiveSessions returns the set of live session slugs for user u.
func (m *Manager) LiveSessions(u *user.User) map[string]bool {
	set := map[string]bool{}
	for _, li := range m.List(u) {
		set[li.ID] = true
	}
	return set
}

// SetLabel sets the display label (tmux @websh_label option) of a session.
func (m *Manager) SetLabel(u *user.User, id, label string) error {
	uid, _, _, err := credentials(u)
	if err != nil {
		return err
	}
	return m.tmuxAsUser(u, "set-option", "-t", SessionName(uid, id), "@websh_label", label).Run()
}

// Kill terminates a session and forgets it.
func (m *Manager) Kill(u *user.User, id string) error {
	uid, _, _, err := credentials(u)
	if err != nil {
		return err
	}
	name := SessionName(uid, id)
	m.mu.Lock()
	delete(m.sessions, name)
	m.mu.Unlock()
	return m.tmuxAsUser(u, "kill-session", "-t", name).Run()
}

// NextBashID returns the smallest positive integer id not currently in use.
func (m *Manager) NextBashID(u *user.User) string {
	used := m.LiveSessions(u)
	for i := 1; ; i++ {
		id := strconv.Itoa(i)
		if !used[id] {
			return id
		}
	}
}
