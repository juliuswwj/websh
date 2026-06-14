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

// SessionSpec is one selectable session in the picker.
type SessionSpec struct {
	ID         string   `yaml:"id"`
	Type       string   `yaml:"type"` // "local" | "ssh"
	Name       string   `yaml:"name"`
	Host       string   `yaml:"host,omitempty"`
	User       string   `yaml:"user,omitempty"`
	Port       int      `yaml:"port,omitempty"`
	SSHOptions []string `yaml:"ssh_options,omitempty"`
}

// Config is the parsed websh.yaml.
type Config struct {
	OTPSecret   string        `yaml:"otp_secret"`
	DisplayName string        `yaml:"display_name,omitempty"`
	Sessions    []SessionSpec `yaml:"sessions"`
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
	seen := map[string]bool{}
	for i := range c.Sessions {
		s := &c.Sessions[i]
		if !idRe.MatchString(s.ID) {
			return fmt.Errorf("session id %q must match [A-Za-z0-9_-]+", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate session id %q", s.ID)
		}
		seen[s.ID] = true
		switch s.Type {
		case "local":
			// nothing else required
		case "ssh":
			if !hostRe.MatchString(s.Host) {
				return fmt.Errorf("session %q: host %q must match [A-Za-z0-9._-]+", s.ID, s.Host)
			}
			if s.User != "" && !hostRe.MatchString(s.User) {
				return fmt.Errorf("session %q: invalid ssh user %q", s.ID, s.User)
			}
			if s.Port != 0 && (s.Port < 1 || s.Port > 65535) {
				return fmt.Errorf("session %q: port out of range", s.ID)
			}
			for _, opt := range s.SSHOptions {
				low := strings.ToLower(opt)
				for _, bad := range dangerousSSHOpt {
					if strings.Contains(low, bad) {
						return fmt.Errorf("session %q: ssh option %q is not allowed", s.ID, opt)
					}
				}
			}
		default:
			return fmt.Errorf("session %q: unknown type %q (want local|ssh)", s.ID, s.Type)
		}
	}
	return nil
}

// Find returns the session spec with the given id, or false.
func (c *Config) Find(id string) (SessionSpec, bool) {
	for _, s := range c.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return SessionSpec{}, false
}
