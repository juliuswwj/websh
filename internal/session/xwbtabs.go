package session

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
)

// xwb tab persistence. x-workbench does NOT persist bash tab_ids, so websh keeps
// them here, per user, so a bash shell can be re-attached (same tab_id) after the
// local proxy/tmux/host restarts. Claude tab_ids are also recorded for a uniform
// proxy re-spawn, though they are additionally re-listable from the upstream
// cicada service.
//
// File: ~/.cache/websh/xwb-tabs.json
//   { "<remote-id>": [ {"idx":0,"kind":"bash","tab_id":"mob-bash-…","name":"…"} ] }

var xwbTabsMu sync.Mutex

// XWBTab is one persisted xwb tab on a given remote.
type XWBTab struct {
	Idx   int    `json:"idx"`
	Kind  string `json:"kind"` // "bash" | "claude"
	TabID string `json:"tab_id"`
	Name  string `json:"name"`
}

func xwbTabsPath(u *user.User) string {
	return filepath.Join(u.HomeDir, ".cache", "websh", "xwb-tabs.json")
}

func xwbTabsLoad(u *user.User) map[string][]XWBTab {
	store := map[string][]XWBTab{}
	data, err := os.ReadFile(xwbTabsPath(u))
	if err != nil {
		return store
	}
	_ = json.Unmarshal(data, &store)
	if store == nil {
		store = map[string][]XWBTab{}
	}
	return store
}

func xwbTabsSave(u *user.User, store map[string][]XWBTab) error {
	cache := filepath.Join(u.HomeDir, ".cache")
	dir := filepath.Join(cache, "websh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := xwbTabsPath(u)
	data, _ := json.MarshalIndent(store, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if os.Geteuid() == 0 {
		if uid, err := strconv.Atoi(u.Uid); err == nil {
			gid, _ := strconv.Atoi(u.Gid)
			_ = os.Chown(cache, uid, gid)
			_ = os.Chown(dir, uid, gid)
			_ = os.Chown(path, uid, gid)
		}
	}
	return nil
}

// XWBTabsFor returns the recorded tabs for one remote.
func (m *Manager) XWBTabsFor(u *user.User, remoteID string) []XWBTab {
	xwbTabsMu.Lock()
	defer xwbTabsMu.Unlock()
	return xwbTabsLoad(u)[remoteID]
}

// xwbTabAdd records a newly created tab.
func xwbTabAdd(u *user.User, remoteID string, tab XWBTab) error {
	xwbTabsMu.Lock()
	defer xwbTabsMu.Unlock()
	store := xwbTabsLoad(u)
	store[remoteID] = append(store[remoteID], tab)
	return xwbTabsSave(u, store)
}

// xwbTabKind looks up the kind of a persisted tab by its tab_id (across remotes).
func xwbTabKind(u *user.User, tabID string) string {
	xwbTabsMu.Lock()
	defer xwbTabsMu.Unlock()
	for _, tabs := range xwbTabsLoad(u) {
		for _, t := range tabs {
			if t.TabID == tabID {
				return t.Kind
			}
		}
	}
	return ""
}

// XWBTabRemove drops a tab record (by tab_id) for one remote.
func (m *Manager) XWBTabRemove(u *user.User, remoteID, tabID string) {
	xwbTabsMu.Lock()
	defer xwbTabsMu.Unlock()
	store := xwbTabsLoad(u)
	lst := store[remoteID]
	out := lst[:0]
	for _, t := range lst {
		if t.TabID != tabID {
			out = append(out, t)
		}
	}
	store[remoteID] = out
	_ = xwbTabsSave(u, store)
}
