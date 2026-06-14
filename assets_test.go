package websh

import (
	"io/fs"
	"testing"
)

// TestStaticFSEmbedded ensures the web UI is actually baked into the binary.
func TestStaticFSEmbedded(t *testing.T) {
	sub, err := fs.Sub(StaticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"index.html", "app.js", "service-worker.js", "manifest.json",
		"vendor/xterm.js", "icons/icon-192.png",
	} {
		if _, err := fs.Stat(sub, f); err != nil {
			t.Errorf("embedded asset missing: %s (%v)", f, err)
		}
	}
}
