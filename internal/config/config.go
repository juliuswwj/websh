// Package config loads and validates a user's ~/.config/websh.yaml.
//
// The daemon runs as root and reads each logging-in user's config out of their
// own home directory (resolved via the passwd database, never a hardcoded
// /home/<user> — homes may live elsewhere, e.g. /data/home/<user>). If the file
// does not exist the user is not allowed to log in.
package config

import (
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoConfig is returned when the user has no ~/.config/websh.yaml. Login must
// be rejected in this case.
var ErrNoConfig = errors.New("websh config not found")

// Remote is a configured quick-connect target shown in the picker. Local shells
// are created ad-hoc ("+ new bash"), so they are not configured here.
//
// Two kinds (Type): "ssh" (the default) connects to Host over ssh; "xwb"
// connects to a server inside x-workbench, named by its IP — server_id and
// credential_id are resolved at runtime via the xwb API, not configured.
//
// The session id is the slug-safe handle baked into the tmux session name
// "<n>@<id>" (not the user-facing label); it defaults to slug(host) for ssh and
// slug(ip) for xwb. Name is the human display label.
type Remote struct {
	ID         string   `yaml:"id,omitempty"` // optional handle; default = slug(host|ip)
	Name       string   `yaml:"name,omitempty"`
	Type       string   `yaml:"type,omitempty"` // "ssh" (default) | "xwb"
	Host       string   `yaml:"host,omitempty"` // ssh
	User       string   `yaml:"user,omitempty"` // ssh
	Port       int      `yaml:"port,omitempty"` // ssh
	SSHOptions []string `yaml:"ssh_options,omitempty"`

	IP           string `yaml:"ip,omitempty"`            // xwb: the server's IP within x-workbench
	CredentialID int    `yaml:"credential_id,omitempty"` // xwb: optional override; else auto-resolved
}

// IsXWB reports whether this remote is an x-workbench target.
func (r Remote) IsXWB() bool { return r.Type == "xwb" }

// XWBConfig holds the x-workbench account + endpoint shared by all xwb remotes.
type XWBConfig struct {
	Host     string `yaml:"host"` // upstream host:port, e.g. 172.60.1.35:9630
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
}

// Config is the parsed websh.yaml.
type Config struct {
	OTPSecret   string     `yaml:"otp_secret"`
	DisplayName string     `yaml:"display_name,omitempty"`
	XWB         *XWBConfig `yaml:"xwb,omitempty"`
	Remotes     []Remote   `yaml:"remotes"`
}

var (
	idRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// dangerous ssh options that can execute arbitrary local commands.
var dangerousSSHOpt = []string{"proxycommand", "localcommand", "permitlocalcommand"}

// Path returns the config path for a resolved passwd user.
func Path(u *user.User) string {
	return filepath.Join(u.HomeDir, ".config", "websh.yaml")
}

// LoadForUser resolves the user via the passwd database, then loads and
// validates their config. Returns ErrNoConfig if the file is absent.
func LoadForUser(username string) (*Config, *user.User, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, nil, fmt.Errorf("unknown user: %w", err)
	}
	cfg, err := Load(Path(u))
	if err != nil {
		return nil, u, err
	}
	return cfg, u, nil
}

// Load reads and validates a config file at the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoConfig
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.OTPSecret) == "" {
		return errors.New("otp_secret is required")
	}
	// otp_secret must be valid base32 (Google Authenticator compatible).
	norm := strings.ToUpper(strings.ReplaceAll(c.OTPSecret, " ", ""))
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(norm, "=")); err != nil {
		return fmt.Errorf("otp_secret is not valid base32: %w", err)
	}
	if c.XWB != nil {
		if strings.TrimSpace(c.XWB.Host) == "" || strings.TrimSpace(c.XWB.Email) == "" || c.XWB.Password == "" {
			return errors.New("xwb section requires host, email and password")
		}
	}
	seen := map[string]bool{}
	for i := range c.Remotes {
		r := &c.Remotes[i]
		if r.Type == "" {
			r.Type = "ssh"
		}
		switch r.Type {
		case "ssh":
			if err := c.validateSSH(r); err != nil {
				return err
			}
		case "xwb":
			if err := c.validateXWB(r); err != nil {
				return err
			}
		default:
			return fmt.Errorf("remote %q: unknown type %q (want ssh or xwb)", r.ID, r.Type)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate remote id %q (set a distinct id:)", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

func (c *Config) validateSSH(r *Remote) error {
	if r.IP != "" || r.CredentialID != 0 {
		return fmt.Errorf("remote %q: ip/credential_id are only valid for type: xwb", r.ID)
	}
	if !hostRe.MatchString(r.Host) {
		return fmt.Errorf("remote host %q must match [A-Za-z0-9._-]+", r.Host)
	}
	if r.ID == "" {
		r.ID = slugify(r.Host)
	}
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("remote id %q must match [A-Za-z0-9_-]+", r.ID)
	}
	if r.Name == "" {
		r.Name = r.Host
	}
	if r.User != "" && !hostRe.MatchString(r.User) {
		return fmt.Errorf("remote %q: invalid ssh user %q", r.ID, r.User)
	}
	if r.Port != 0 && (r.Port < 1 || r.Port > 65535) {
		return fmt.Errorf("remote %q: port out of range", r.ID)
	}
	for _, opt := range r.SSHOptions {
		low := strings.ToLower(opt)
		for _, bad := range dangerousSSHOpt {
			if strings.Contains(low, bad) {
				return fmt.Errorf("remote %q: ssh option %q is not allowed", r.ID, opt)
			}
		}
	}
	return nil
}

func (c *Config) validateXWB(r *Remote) error {
	if c.XWB == nil {
		return fmt.Errorf("remote %q: type xwb requires a top-level xwb: section", r.ID)
	}
	if r.Host != "" || r.User != "" || r.Port != 0 || len(r.SSHOptions) > 0 {
		return fmt.Errorf("remote %q: host/user/port/ssh_options are only valid for type: ssh", r.ID)
	}
	if !hostRe.MatchString(r.IP) {
		return fmt.Errorf("remote %q: ip %q must match [A-Za-z0-9._-]+", r.ID, r.IP)
	}
	if r.ID == "" {
		r.ID = slugify(r.IP)
	}
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("remote id %q must match [A-Za-z0-9_-]+", r.ID)
	}
	if r.Name == "" {
		r.Name = r.IP
	}
	if r.CredentialID < 0 {
		return fmt.Errorf("remote %q: credential_id must be positive", r.ID)
	}
	return nil
}

// slugify maps a host to a tmux-safe session id ([A-Za-z0-9_-]).
func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "remote"
	}
	return b.String()
}

// ValidID reports whether id is a usable session id ([A-Za-z0-9_-]+).
func ValidID(id string) bool { return idRe.MatchString(id) }

// FindRemote returns the remote with the given (derived) id, or false.
func (c *Config) FindRemote(id string) (Remote, bool) {
	for _, r := range c.Remotes {
		if r.ID == id {
			return r, true
		}
	}
	return Remote{}, false
}
