// Command websh-notify sends an attention notification to the websh daemon for
// the current terminal session. It is meant to be called from shell scripts or
// from claude-code's Notification hook (which delivers a JSON event on stdin).
//
// It reads WEBSH_SESSION, WEBSH_NOTIFY_TOKEN and WEBSH_NOTIFY_URL from the
// environment (injected by websh when the shell was spawned). The daemon
// decides whether to push (PWA backgrounded) or show in-page (foregrounded).
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
	"strings"
	"time"
)

func main() {
	from := flag.String("from", "", "source label (e.g. claude); also reads a JSON event on stdin")
	flag.Parse()

	session := os.Getenv("WEBSH_SESSION")
	token := os.Getenv("WEBSH_NOTIFY_TOKEN")
	url := os.Getenv("WEBSH_NOTIFY_URL")
	if session == "" || url == "" {
		fmt.Fprintln(os.Stderr, "websh-notify: not inside a websh session (WEBSH_SESSION/WEBSH_NOTIFY_URL unset)")
		os.Exit(1)
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

	body, _ := json.Marshal(map[string]string{"session": session, "token": token, "message": msg})
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
