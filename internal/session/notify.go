package session

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// WriteNotifyFile writes the websh-notify config (daemon URL + the user's token)
// to ~/.cache/websh/notify, owned by the user, mode 0600. This lets websh-notify
// work from ANY of the user's shells — including tmux sessions not created by
// websh — without per-session environment injection.
func WriteNotifyFile(u *user.User, url, token string) error {
	cache := filepath.Join(u.HomeDir, ".cache")
	dir := filepath.Join(cache, "websh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "notify")
	data, _ := json.Marshal(map[string]string{"url": url, "token": token})
	if err := os.WriteFile(path, data, 0o600); err != nil {
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
