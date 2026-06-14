package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "websh.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
display_name: "Tester"
sessions:
  - id: local
    type: local
    name: "Local"
  - id: gpu01
    type: ssh
    name: "GPU"
    host: "gpu01.internal"
    user: "deploy"
    port: 22
    ssh_options: ["-o", "ServerAliveInterval=30"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(cfg.Sessions))
	}
	if _, ok := cfg.Find("gpu01"); !ok {
		t.Fatal("Find gpu01 failed")
	}
}

func TestNoConfig(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != ErrNoConfig {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
}

func TestRejectBadOTP(t *testing.T) {
	p := writeCfg(t, "otp_secret: \"not base32 !!!\"\nsessions: []\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for bad otp_secret")
	}
}

func TestRejectDangerousSSHOption(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
sessions:
  - id: bad
    type: ssh
    host: "h"
    ssh_options: ["-o", "ProxyCommand=nc evil 1"]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected ProxyCommand to be rejected")
	}
}

func TestRejectBadHost(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
sessions:
  - id: bad
    type: ssh
    host: "h; rm -rf /"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected bad host to be rejected")
	}
}

func TestRejectBadID(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
sessions:
  - id: "bad id"
    type: local
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected bad id to be rejected")
	}
}
