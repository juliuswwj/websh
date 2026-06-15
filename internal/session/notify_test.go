package session

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestWriteNotifyFile(t *testing.T) {
	dir := t.TempDir()
	u := &user.User{Uid: "1000", Gid: "1000", Username: "x", HomeDir: dir}
	if err := WriteNotifyFile(u, "http://127.0.0.1:9631/internal/notify", "tok123"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".cache", "websh", "notify")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(p)
	var c struct{ URL, Token string }
	if json.Unmarshal(data, &c) != nil || c.URL == "" || c.Token != "tok123" {
		t.Fatalf("bad content: %s", data)
	}
}
