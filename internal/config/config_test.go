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
remotes:
  - host: "gpu01.internal"
    user: "deploy"
    port: 22
    ssh_options: ["-o", "ServerAliveInterval=30"]
  - name: "Box 2"
    host: "10.0.0.5"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Remotes) != 2 {
		t.Fatalf("want 2 remotes, got %d", len(cfg.Remotes))
	}
	// id derived from host, name defaulted to host.
	r, ok := cfg.FindRemote("gpu01_internal")
	if !ok {
		t.Fatalf("FindRemote(gpu01_internal) failed; ids: %+v", cfg.Remotes)
	}
	if r.Name != "gpu01.internal" {
		t.Fatalf("default name = %q, want host", r.Name)
	}
	// IP host slugified.
	if _, ok := cfg.FindRemote("10_0_0_5"); !ok {
		t.Fatal("FindRemote(10_0_0_5) failed")
	}
}

func TestExplicitIDOverride(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - id: gpu
    host: "gpu01.internal"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.FindRemote("gpu"); !ok {
		t.Fatal("explicit id override not honored")
	}
}

func TestNoConfig(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != ErrNoConfig {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
}

func TestRejectBadOTP(t *testing.T) {
	p := writeCfg(t, "otp_secret: \"not base32 !!!\"\nremotes: []\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for bad otp_secret")
	}
}

func TestRejectDangerousSSHOption(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - host: "h"
    ssh_options: ["-o", "ProxyCommand=nc evil 1"]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected ProxyCommand to be rejected")
	}
}

func TestRejectBadHost(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - host: "h; rm -rf /"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected bad host to be rejected")
	}
}

func TestRejectDuplicateDerivedID(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - host: "same.host"
  - host: "same.host"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected duplicate derived id to be rejected")
	}
}

func TestXWBValid(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
xwb:
  host: "172.60.1.35:9630"
  email: "you@example.com"
  password: "secret"
remotes:
  - type: xwb
    ip: "10.0.0.5"
  - type: xwb
    ip: "10.0.0.6"
    id: gpu6
    name: "GPU 6"
    credential_id: 219
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// id defaults to slugified ip; name defaults to ip.
	r, ok := cfg.FindRemote("10_0_0_5")
	if !ok {
		t.Fatalf("FindRemote(10_0_0_5) failed; ids: %+v", cfg.Remotes)
	}
	if !r.IsXWB() || r.Name != "10.0.0.5" {
		t.Fatalf("xwb remote defaults wrong: %+v", r)
	}
	r6, ok := cfg.FindRemote("gpu6")
	if !ok || r6.CredentialID != 219 || r6.Name != "GPU 6" {
		t.Fatalf("explicit xwb remote wrong: %+v", r6)
	}
}

func TestXWBRequiresSection(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - type: xwb
    ip: "10.0.0.5"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected xwb remote without xwb: section to be rejected")
	}
}

func TestXWBSectionRequiresCreds(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
xwb:
  host: "172.60.1.35:9630"
  email: ""
  password: "x"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected xwb section without email to be rejected")
	}
}

func TestRejectCrossTypeFields(t *testing.T) {
	// ssh remote must not carry xwb-only fields.
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - host: "h.internal"
    ip: "10.0.0.9"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected ip on an ssh remote to be rejected")
	}
	// xwb remote must not carry ssh-only fields.
	p2 := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
xwb:
  host: "172.60.1.35:9630"
  email: "you@example.com"
  password: "secret"
remotes:
  - type: xwb
    ip: "10.0.0.5"
    user: "deploy"
`)
	if _, err := Load(p2); err == nil {
		t.Fatal("expected ssh user on an xwb remote to be rejected")
	}
}

func TestUnknownTypeRejected(t *testing.T) {
	p := writeCfg(t, `
otp_secret: "JBSWY3DPEHPK3PXP"
remotes:
  - type: telnet
    host: "h"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected unknown remote type to be rejected")
	}
}
