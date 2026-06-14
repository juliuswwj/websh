package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pquerna/otp/totp"

	"websh/internal/config"
)

// TestWriteConfigValidates ensures `websh config` writes a file the server's
// own loader accepts.
func TestWriteConfigValidates(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "websh", AccountName: "alice@host"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		OTPSecret: key.Secret(),
		Remotes:   []config.Remote{{Host: "gpu01.internal", User: "deploy"}},
	}
	p := filepath.Join(t.TempDir(), "websh.yaml")
	if err := writeConfig(p, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	got, err := config.Load(p)
	if err != nil {
		t.Fatalf("generated config rejected by loader: %v", err)
	}
	if got.OTPSecret != cfg.OTPSecret || len(got.Remotes) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestOtpauthURI(t *testing.T) {
	u := otpauthURI("websh", "alice@host", "ABC234")
	for _, want := range []string{"otpauth://totp/", "issuer=websh", "secret=ABC234"} {
		if !strings.Contains(u, want) {
			t.Fatalf("uri %q missing %q", u, want)
		}
	}
}
