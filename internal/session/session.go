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

	"websh/internal/config"
)

var errNeedRoot = errors.New("websh must run as root to spawn sessions for other users")

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
func (m *Manager) Spawn(spec config.SessionSpec, u *user.User, cols, rows uint16) (*Client, error) {
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

// Resize updates the PTY window size; the kernel signals SIGWINCH and tmux follows.
func (c *Client) Resize(cols, rows uint16) error {
	return pty.Setsize(c.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
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

func buildArgv(spec config.SessionSpec, name string) []string {
	base := []string{"tmux", "new-session", "-A", "-s", name}
	if spec.Type != "ssh" {
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

// LiveSessions returns the set of websh session slugs currently alive for user u.
func (m *Manager) LiveSessions(u *user.User) map[string]bool {
	uid, _, _, err := credentials(u)
	if err != nil {
		return nil
	}
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	cmd.Dir = u.HomeDir
	cmd.Env = []string{"HOME=" + u.HomeDir, "PATH=/usr/local/bin:/usr/bin:/bin"}
	if os.Geteuid() == 0 {
		if cred := credOf(uid, u); cred != nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil // no server / no sessions
	}
	live := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if su, slug, ok := ParseSessionName(strings.TrimSpace(line)); ok && su == strconv.FormatUint(uint64(uid), 10) {
			live[slug] = true
		}
	}
	return live
}

func credOf(uid uint32, u *user.User) *syscall.Credential {
	_, gid, groups, err := credentials(u)
	if err != nil {
		return nil
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}
}
