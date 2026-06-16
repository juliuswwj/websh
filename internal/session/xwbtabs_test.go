package session

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestXWBTabsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	u := &user.User{Uid: "1000", Gid: "1000", Username: "x", HomeDir: dir}
	m := NewManager()

	if got := m.XWBTabsFor(u, "gpu"); len(got) != 0 {
		t.Fatalf("empty store should have no tabs, got %d", len(got))
	}

	if err := xwbTabAdd(u, "gpu", XWBTab{Idx: 0, Kind: "bash", TabID: "mob-bash-aaa", Name: "0@gpu"}); err != nil {
		t.Fatal(err)
	}
	if err := xwbTabAdd(u, "gpu", XWBTab{Idx: 1, Kind: "claude", TabID: "tab-claude-1", Name: "1@gpu"}); err != nil {
		t.Fatal(err)
	}
	if err := xwbTabAdd(u, "other", XWBTab{Idx: 0, Kind: "bash", TabID: "mob-bash-bbb", Name: "0@other"}); err != nil {
		t.Fatal(err)
	}

	// File exists with 0600.
	p := filepath.Join(dir, ".cache", "websh", "xwb-tabs.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}

	tabs := m.XWBTabsFor(u, "gpu")
	if len(tabs) != 2 || tabs[0].TabID != "mob-bash-aaa" || tabs[1].Kind != "claude" {
		t.Fatalf("XWBTabsFor(gpu) = %+v", tabs)
	}

	// Kind lookup is global across remotes.
	if k := xwbTabKind(u, "tab-claude-1"); k != "claude" {
		t.Fatalf("xwbTabKind = %q, want claude", k)
	}
	if k := xwbTabKind(u, "nope"); k != "" {
		t.Fatalf("xwbTabKind(unknown) = %q, want empty", k)
	}

	// Remove only the matching tab on the matching remote.
	m.XWBTabRemove(u, "gpu", "mob-bash-aaa")
	tabs = m.XWBTabsFor(u, "gpu")
	if len(tabs) != 1 || tabs[0].TabID != "tab-claude-1" {
		t.Fatalf("after remove, XWBTabsFor(gpu) = %+v", tabs)
	}
	if got := m.XWBTabsFor(u, "other"); len(got) != 1 {
		t.Fatalf("other remote should be untouched, got %+v", got)
	}
}
