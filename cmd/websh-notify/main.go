// Command websh-notify sends an attention notification to the websh daemon for
// the current user. It is meant to be called from shell scripts or from
// claude-code's Notification hook (which delivers a JSON event on stdin).
//
// It reads ~/.cache/websh/notify ({url, token}, written by websh at login), so
// it works in ANY of the user's shells. The daemon decides whether to push (PWA
// backgrounded) or show in-page (foregrounded).
//
// Usage:
//
//	websh-notify "build finished"
//	echo '{"message":"needs approval"}' | websh-notify --from claude
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	from := flag.String("from", "", "source label (e.g. claude); also reads a JSON event on stdin")
	flag.Parse()

	url, token := loadNotifyConfig()
	if url == "" || token == "" {
		fmt.Fprintln(os.Stderr, "websh-notify: not configured (no ~/.cache/websh/notify — log in via websh once)")
		os.Exit(1)
	}
	uname := ""
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}

	msg := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if msg == "" {
		msg = stdinMessage()
	}
	if msg == "" {
		msg = "终端需要你的关注"
	}
	if *from != "" {
		msg = "[" + *from + "] " + msg
	}

	body, _ := json.Marshal(map[string]string{"user": uname, "token": token, "message": msg})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "websh-notify:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "websh-notify: %s: %s\n", resp.Status, strings.TrimSpace(string(b)))
		os.Exit(1)
	}
}

// loadNotifyConfig reads {url, token} from ~/.cache/websh/notify (falling back
// to the legacy environment variables).
func loadNotifyConfig() (url, token string) {
	home, _ := os.UserHomeDir()
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".cache", "websh", "notify")); err == nil {
			var c struct{ URL, Token string }
			if json.Unmarshal(data, &c) == nil {
				url, token = c.URL, c.Token
			}
		}
	}
	if url == "" {
		url = os.Getenv("WEBSH_NOTIFY_URL")
	}
	if token == "" {
		token = os.Getenv("WEBSH_NOTIFY_TOKEN")
	}
	return url, token
}

// stdinMessage reads a message from stdin: either a claude-code Notification
// hook JSON event ({"message": "..."}) or plain text. Returns "" if no stdin.
func stdinMessage() string {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) != 0 {
		return "" // no piped input
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return ""
	}
	var ev struct {
		Message string `json:"message"`
		Title   string `json:"title"`
	}
	if json.Unmarshal(data, &ev) == nil && ev.Message != "" {
		return ev.Message
	}
	return strings.TrimSpace(string(data))
}
