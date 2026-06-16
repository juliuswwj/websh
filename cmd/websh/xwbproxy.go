package main

// The xwb-proxy subcommand is the per-tab websocket bridge for the x-workbench
// backend. websh spawns it inside a tmux proxy session "<n>@<id>" (like ssh is
// spawned for ssh remotes): the pane PTY is the terminal, and this process pumps
// it to/from the xwb websocket. It runs AS the user, reads the xwb account from
// ~/.config/websh.yaml, and — because the xwb websocket is unstable — reconnects
// automatically (reusing the same tab_id), refreshing the JWT when it expires.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"websh/internal/config"
	"websh/internal/xwb"
)

type frame struct {
	typ  websocket.MessageType
	data []byte
}

// runXWBProxy implements `websh xwb-proxy …`. It never returns on success (it
// loops reconnecting); it returns by os.Exit on a fatal configuration error.
func runXWBProxy(args []string) {
	fs := flag.NewFlagSet("xwb-proxy", flag.ExitOnError)
	kind := fs.String("kind", "bash", `tab kind: "bash" or "claude"`)
	serverID := fs.Int("server-id", 0, "xwb server_id")
	credentialID := fs.Int("credential-id", 0, "xwb credential_id (bash only)")
	tabID := fs.String("tab-id", "", "xwb tab_id (the shell handle)")
	_ = fs.Parse(args)

	if *tabID == "" || (*kind != "bash" && *kind != "claude") {
		fmt.Fprintln(os.Stderr, "xwb-proxy: --tab-id required; --kind must be bash|claude")
		os.Exit(2)
	}

	u, err := user.Current()
	if err != nil {
		fatalLine("无法确定当前用户: " + err.Error())
	}
	cfg, err := config.Load(config.Path(u))
	if err != nil {
		fatalLine("加载 websh.yaml 失败: " + err.Error())
	}
	if cfg.XWB == nil {
		fatalLine("websh.yaml 缺少 xwb: 配置段")
	}
	client := xwb.New(xwb.Creds{Host: cfg.XWB.Host, Email: cfg.XWB.Email, Password: cfg.XWB.Password}, u.HomeDir, nil)

	// Put the pane PTY into raw mode (like `ssh -tt`): the remote shell echoes,
	// so local echo must be off (else every keystroke shows twice), and CR must
	// NOT be translated to NL — otherwise Enter arrives as a newline and TUIs
	// like claude keep adding lines instead of submitting. Best-effort: if stdin
	// is not a tty (e.g. run outside tmux), carry on without it.
	if _, err := setRawTTY(int(os.Stdin.Fd())); err != nil {
		log.Printf("xwb-proxy: raw mode unavailable: %v", err)
	}

	// Persistent stdin reader: the pane PTY's input. Bytes typed while
	// disconnected queue here (bounded) and flush on reconnect.
	stdinCh := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				stdinCh <- chunk
			}
			if err != nil {
				close(stdinCh)
				return
			}
		}
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	ctx := context.Background()
	const minBackoff, maxBackoff = 500 * time.Millisecond, 10 * time.Second
	backoff := minBackoff
	first := true
	for {
		if first {
			statusLine("连接中…")
			first = false
		}
		token, err := client.Token(ctx)
		if err != nil {
			statusLine("登录失败: " + err.Error())
			time.Sleep(backoff)
			backoff = grow(backoff, maxBackoff)
			continue
		}
		var wsURL string
		if *kind == "claude" {
			wsURL = client.ClaudeWSURL(*tabID, token)
		} else {
			wsURL = client.TermWSURL(*serverID, *credentialID, *tabID, token)
		}
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		conn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{
			HTTPHeader: map[string][]string{"Origin": {client.Origin()}},
		})
		dcancel()
		if err != nil {
			statusLine("连接失败，重试中…")
			time.Sleep(backoff)
			backoff = grow(backoff, maxBackoff)
			continue
		}
		backoff = minBackoff // connected: reset
		conn.SetReadLimit(8 << 20)

		code := pump(ctx, conn, stdinCh, winch)
		_ = conn.Close(websocket.StatusNormalClosure, "")

		if code == 4401 {
			// JWT rejected (expired/invalid): force a fresh login next round.
			_, _ = client.ForceLogin(ctx)
		}
		statusLine("连接断开，重连中…")
		time.Sleep(backoff)
		backoff = grow(backoff, maxBackoff)
	}
}

// pump runs one websocket connection until it errors, serializing all websocket
// writes through a single goroutine. It returns the websocket close code (or -1).
func pump(ctx context.Context, conn *websocket.Conn, stdinCh <-chan []byte, winch <-chan os.Signal) websocket.StatusCode {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make(chan frame, 256)
	var wg sync.WaitGroup

	// Single writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-out:
				wctx, c := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, f.typ, f.data)
				c()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// stdin (pane input) -> websocket binary.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-stdinCh:
				if !ok {
					cancel()
					return
				}
				select {
				case out <- frame{websocket.MessageBinary, chunk}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Window size: send current size now, and on every SIGWINCH.
	wg.Add(1)
	go func() {
		defer wg.Done()
		sendResize(ctx, out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				sendResize(ctx, out)
			}
		}
	}()

	// websocket -> stdout (this goroutine drives the connection lifetime).
	var code websocket.StatusCode = -1
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			code = websocket.CloseStatus(err)
			break
		}
		if typ == websocket.MessageBinary {
			_, _ = os.Stdout.Write(data)
			continue
		}
		// Text control frame: answer the server's keepalive ping.
		var m struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &m) == nil && m.Type == "ping" {
			select {
			case out <- frame{websocket.MessageText, []byte(`{"type":"pong"}`)}:
			case <-ctx.Done():
			}
		}
	}
	cancel()
	wg.Wait()
	return code
}

func sendResize(ctx context.Context, out chan<- frame) {
	rows, cols, err := pty.Getsize(os.Stdin)
	if err != nil || rows <= 0 || cols <= 0 {
		return
	}
	msg, _ := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	select {
	case out <- frame{websocket.MessageText, msg}:
	case <-ctx.Done():
	}
}

// setRawTTY puts a tty fd into raw mode (no echo, no canonical line buffering,
// no CR/NL translation, no output post-processing), mirroring what `ssh -tt`
// does to its local terminal so the proxy is a transparent byte pipe. Returns
// the previous termios for restoration, or an error if fd is not a tty.
func setRawTTY(fd int) (*syscall.Termios, error) {
	var old syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, syscall.TCSETS, &raw); err != nil {
		return nil, err
	}
	return &old, nil
}

func ioctlTermios(fd int, req uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func grow(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

// statusLine writes a dim status note to the pane (visible to the user).
func statusLine(s string) {
	fmt.Fprintf(os.Stdout, "\r\n\x1b[90m[%s]\x1b[0m\r\n", s)
}

// fatalLine reports a fatal config error to the pane and exits.
func fatalLine(s string) {
	fmt.Fprintf(os.Stdout, "\r\n\x1b[91m[xwb] %s\x1b[0m\r\n", s)
	log.Printf("xwb-proxy: %s", s)
	time.Sleep(2 * time.Second) // let the user read it before the pane closes
	os.Exit(1)
}
