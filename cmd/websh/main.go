// Command websh is a self-contained mobile shell-terminal server: PAM + TOTP
// login against local accounts, tmux-backed sessions (local or SSH) spawned as
// the logged-in user, and Web Push notifications when the PWA is backgrounded.
//
// It must run as root to read each user's ~/.config/websh.yaml and to spawn
// shells as that user.
package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/coder/websocket"

	"websh"
	"websh/internal/auth"
	"websh/internal/bridge"
	"websh/internal/config"
	"websh/internal/presence"
	"websh/internal/push"
	"websh/internal/session"
)

type server struct {
	sessions     *auth.Store
	mgr          *session.Manager
	presence     *presence.Tracker
	push         *push.Store
	files        http.Handler
	staticDir    string
	pamService   string
	secureCookie bool
	notifySecret []byte
	limiter      *limiter
	wsHeartbeat  time.Duration
	notifyURL    string
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "config" {
		runConfig(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	runServe(args)
}

func runServe(args []string) {
	var (
		bind         = flag.String("bind", "127.0.0.1:9631", "listen address")
		staticDir    = flag.String("static", "", "serve web UI from this directory instead of the embedded assets (dev)")
		secretsPath  = flag.String("secrets", "/var/lib/websh/secrets.json", "VAPID/notify secrets file")
		pushPath     = flag.String("push-store", "/var/lib/websh/push_subs.json", "push subscription store")
		pamService   = flag.String("pam-service", "websh", "PAM service name")
		mailto       = flag.String("vapid-mailto", "mailto:admin@websh.local", "VAPID subscriber contact")
		secureCookie = flag.Bool("secure-cookie", false, "set Secure flag on the session cookie (enable behind HTTPS)")
		sessionTTL   = flag.Duration("session-ttl", 7*24*time.Hour, "web login session lifetime")
		sessionStore = flag.String("session-store", "/run/websh/sessions.json", "persist web sessions here so they survive a restart (empty = in-memory only)")
		wsHeartbeat  = flag.Duration("ws-heartbeat", 15*time.Second, "websocket heartbeat interval; must be shorter than any reverse-proxy idle timeout")
	)
	_ = flag.CommandLine.Parse(args)

	if os.Geteuid() != 0 {
		log.Printf("WARNING: not running as root — only sessions for the current user (%d) will work, and PAM auth needs root", os.Getuid())
	}

	sec, err := loadSecrets(*secretsPath)
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}

	files, staticSrc := staticHandler(*staticDir)

	if *sessionStore != "" {
		if err := os.MkdirAll(filepath.Dir(*sessionStore), 0o700); err != nil {
			log.Printf("session store dir not writable (%v); sessions will be in-memory only", err)
			*sessionStore = ""
		}
	}

	srv := &server{
		sessions:     auth.NewStore(*sessionTTL, *sessionStore),
		presence:     presence.New(),
		push:         push.NewStore(*pushPath, sec.VAPIDPrivate, sec.VAPIDPublic, *mailto),
		files:        files,
		staticDir:    staticSrc,
		pamService:   *pamService,
		secureCookie: *secureCookie,
		notifySecret: sec.notifySecretBytes(),
		limiter:      newLimiter(5, 5*time.Minute),
		wsHeartbeat:  *wsHeartbeat,
		mgr:          session.NewManager(),
		notifyURL:    "http://" + loopbackAddr(*bind) + "/internal/notify",
	}

	stop := make(chan struct{})
	go srv.sessions.GC(stop)

	httpSrv := &http.Server{Addr: *bind, Handler: srv.routes()}

	go func() {
		log.Printf("websh listening on %s (web ui: %s)", *bind, staticSrc)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/sessions/rename", s.handleRename)
	mux.HandleFunc("POST /api/sessions/kill", s.handleKill)
	mux.HandleFunc("GET /api/push/vapid-public-key", s.handleVAPIDKey)
	mux.HandleFunc("POST /api/push/subscribe", s.handleSubscribe)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("POST /internal/notify", s.handleInternalNotify)
	mux.HandleFunc("GET /", s.handleStatic)
	return s.logged(mux)
}

// logged wraps a handler with access logging. The wrapper stays transparent to
// websocket hijacking via Unwrap()/Hijack() so it doesn't break the upgrade.
func (s *server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &logRW{ResponseWriter: w, code: 200}
		start := time.Now()
		next.ServeHTTP(lw, r)
		// Skip the noisy cached static assets; log API/ws/navigation/errors.
		if lw.code >= 400 || isInteresting(r.URL.Path) {
			log.Printf("%s %s -> %d (%s) ip=%s", r.Method, r.URL.Path, lw.code, time.Since(start).Round(time.Millisecond), clientIP(r))
		}
	})
}

func isInteresting(p string) bool {
	if p == "/" {
		return true
	}
	return p == "/ws" || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/") || strings.HasPrefix(p, "/internal/")
}

type logRW struct {
	http.ResponseWriter
	code int
}

func (l *logRW) WriteHeader(c int)           { l.code = c; l.ResponseWriter.WriteHeader(c) }
func (l *logRW) Unwrap() http.ResponseWriter { return l.ResponseWriter }
func (l *logRW) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (l *logRW) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := l.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter is not a Hijacker")
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- auth ------------------------------------------------------------------

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		httpErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !s.limiter.allow(req.Username) {
		httpErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}

	cfg, u, err := config.LoadForUser(req.Username)
	if errors.Is(err, config.ErrNoConfig) {
		log.Printf("login denied user=%s ip=%s: no config", req.Username, clientIP(r))
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusForbidden, "未配置 websh（缺少 ~/.config/websh.yaml）")
		return
	}
	if err != nil {
		log.Printf("login denied user=%s ip=%s: %v", req.Username, clientIP(r), err)
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusUnauthorized, "登录失败")
		return
	}
	if sh := loginShell(req.Username); badShell(sh) {
		log.Printf("login denied user=%s ip=%s: shell %q not allowed", req.Username, clientIP(r), sh)
		httpErr(w, http.StatusForbidden, "该账号不允许登录")
		return
	}
	// Verify the OTP first, then the password via PAM.
	if !auth.VerifyTOTP(cfg.OTPSecret, req.OTP) {
		log.Printf("login failed user=%s ip=%s: bad TOTP", req.Username, clientIP(r))
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusUnauthorized, "验证码错误")
		return
	}
	if err := auth.PAMAuthenticate(s.pamService, req.Username, req.Password); err != nil {
		log.Printf("login failed user=%s ip=%s: PAM rejected (service=%s)", req.Username, clientIP(r), s.pamService)
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	s.limiter.reset(req.Username)
	log.Printf("login ok user=%s ip=%s", req.Username, clientIP(r))
	sid := s.sessions.New(req.Username, u.Uid)
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookie,
		MaxAge:   int(s.sessions.TTL().Seconds()),
	})
	writeJSON(w, map[string]any{"user": req.Username, "display_name": cfg.DisplayName})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.FromRequest(r)
	if sess == nil {
		httpErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	writeJSON(w, map[string]any{"user": sess.User})
}

// ---- sessions --------------------------------------------------------------

// handleSessions returns ALL of the user's tmux sessions plus the configured
// SSH remotes (for starting new ones).
func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sess, cfg, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	remoteByName := map[string]bool{}
	for _, rmt := range cfg.Remotes {
		remoteByName[rmt.ID] = true
	}
	live := make([]map[string]any, 0)
	for _, li := range s.mgr.List(u) {
		typ := "bash"
		// "<n>@<remoteid>" is a session on a configured SSH remote.
		if at := strings.LastIndex(li.Name, "@"); at >= 0 && remoteByName[li.Name[at+1:]] {
			typ = "ssh"
		}
		live = append(live, map[string]any{"name": li.Name, "type": typ, "attached": li.Attached, "window": li.Window})
	}
	remotes := make([]map[string]any, 0, len(cfg.Remotes))
	for _, rmt := range cfg.Remotes {
		remotes = append(remotes, map[string]any{"id": rmt.ID, "name": rmt.Name, "host": rmt.Host})
	}
	writeJSON(w, map[string]any{"user": sess.User, "display_name": cfg.DisplayName, "live": live, "remotes": remotes})
}

// handleRename renames a tmux session. For a remote session "<prefix>@<id>" only
// the prefix changes ("@<id>" identifies the remote and is preserved); the remote
// tmux session is renamed too so the two stay in sync.
func (s *server) handleRename(w http.ResponseWriter, r *http.Request) {
	_, cfg, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	var req struct{ Target, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	plan, err := planRename(req.Target, req.Name, func(id string) bool { _, ok := cfg.FindRemote(id); return ok })
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if plan.remote {
		rmt, _ := cfg.FindRemote(plan.remoteID)
		spec := session.Spec{ID: rmt.ID, SSH: true, Host: rmt.Host, User: rmt.User, Port: rmt.Port, SSHOptions: rmt.SSHOptions}
		if err := s.mgr.RenameRemote(u, spec, plan.oldPrefix, plan.newPrefix); err != nil {
			httpErr(w, http.StatusBadRequest, "重命名远端会话失败（远端不可达？）")
			return
		}
	}
	if err := s.mgr.Rename(u, req.Target, plan.newName); err != nil {
		httpErr(w, http.StatusBadRequest, "改名失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// renamePlan is the resolved outcome of a rename request.
type renamePlan struct {
	remote    bool
	remoteID  string // remote id (the "@<id>" part), for remote sessions
	oldPrefix string // current remote session name (target's prefix)
	newPrefix string // new remote session name
	newName   string // new local (proxy) session name
}

// planRename resolves a rename, enforcing that "@<id>" is reserved for remote
// sessions: a remote session keeps its "@<id>" suffix and only its prefix
// changes (the prefix may not contain '@'); a local session may not contain '@'
// at all, so it can't masquerade as a remote "<n>@<id>".
func planRename(target, rawName string, isRemote func(string) bool) (renamePlan, error) {
	name := strings.TrimSpace(rawName)
	if at := strings.LastIndex(target, "@"); at >= 0 && isRemote(target[at+1:]) {
		id := target[at+1:]
		newPrefix := name
		if a2 := strings.LastIndex(name, "@"); a2 >= 0 {
			newPrefix = name[:a2] // ignore any @suffix the client sent; @<id> is fixed
		}
		if newPrefix == "" || strings.Contains(newPrefix, "@") {
			return renamePlan{}, errors.New("会话名不能含 @")
		}
		return renamePlan{remote: true, remoteID: id, oldPrefix: target[:at], newPrefix: newPrefix, newName: newPrefix + "@" + id}, nil
	}
	if name == "" || strings.Contains(name, "@") {
		return renamePlan{}, errors.New("本机会话名不能含 @（@ 仅用于远端会话）")
	}
	return renamePlan{newName: name}, nil
}

// handleKill terminates a tmux session.
func (s *server) handleKill(w http.ResponseWriter, r *http.Request) {
	_, _, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	var req struct{ Target string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.mgr.Kill(u, req.Target); err != nil {
		httpErr(w, http.StatusBadRequest, "删除失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// sessionUser resolves the logged-in session, its config and passwd user.
func (s *server) sessionUser(w http.ResponseWriter, r *http.Request) (*auth.Session, *config.Config, *user.User, bool) {
	sess := s.sessions.FromRequest(r)
	if sess == nil {
		httpErr(w, http.StatusUnauthorized, "未登录")
		return nil, nil, nil, false
	}
	cfg, u, err := config.LoadForUser(sess.User)
	if err != nil {
		httpErr(w, http.StatusForbidden, "配置不可用")
		return nil, nil, nil, false
	}
	return sess, cfg, u, true
}

// ---- terminal websocket ----------------------------------------------------

// handleWS is the single terminal websocket: it attaches one top-level tmux
// client for the user and drives it (switch/new) via control frames.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	log.Printf("ws: host=%s origin=%q upgrade=%q ip=%s", r.Host, r.Header.Get("Origin"), r.Header.Get("Upgrade"), clientIP(r))

	sess := s.sessions.FromRequest(r)
	if sess == nil {
		log.Printf("ws: rejected, no session")
		http.Error(w, "未登录", http.StatusUnauthorized)
		return
	}
	cfg, u, err := config.LoadForUser(sess.User)
	if err != nil {
		log.Printf("ws: config load failed user=%s: %v", sess.User, err)
		httpErr(w, http.StatusForbidden, "配置不可用")
		return
	}

	// InsecureSkipVerify: the upgrade comes through a reverse proxy (Origin host
	// != backend Host). CSRF is prevented by the SameSite=Lax session cookie.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("ws: accept failed user=%s: %v", sess.User, err)
		return
	}
	defer c.CloseNow()

	// Make websh-notify work in every shell of this user.
	_ = session.WriteNotifyFile(u, s.notifyURL, s.notifyToken(sess.User))

	client, err := s.mgr.Attach(u, 80, 24)
	if err != nil {
		log.Printf("ws: attach failed user=%s: %v", sess.User, err)
		_ = c.Write(r.Context(), websocket.MessageBinary, []byte("\r\n\x1b[91m启动会话失败: "+err.Error()+"\x1b[0m\r\n"))
		c.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	defer client.Close()
	log.Printf("ws: connected user=%s", sess.User)

	uid := sess.UID
	b := bridge.New(c, client)
	b.Heartbeat = s.wsHeartbeat
	conn := s.presence.Add(uid, b.Send)
	defer s.presence.Remove(conn)
	b.OnPresence = func(state string) { s.presence.SetState(conn, state) }
	b.OnAttention = func() { s.attention(uid, "终端需要你的关注") }
	b.OnSwitch = func(target string) string {
		if err := client.Switch(target); err != nil {
			log.Printf("ws: switch user=%s target=%q: %v", sess.User, target, err)
			return ""
		}
		return target
	}
	b.OnNew = func(kind, id string) string {
		var name string
		var err error
		if kind == "remote" {
			rmt, ok := cfg.FindRemote(id)
			if !ok {
				return ""
			}
			name, err = client.NewRemote(session.Spec{ID: rmt.ID, SSH: true, Host: rmt.Host, User: rmt.User, Port: rmt.Port, SSHOptions: rmt.SSHOptions})
		} else {
			name, err = client.NewBash()
		}
		if err != nil {
			log.Printf("ws: new(%s,%q) user=%s: %v", kind, id, sess.User, err)
			return ""
		}
		return name
	}

	// Tell the client which session it landed on.
	if name := client.CurrentSession(); name != "" {
		frame, _ := json.Marshal(map[string]string{"type": "session", "name": name})
		b.Send(frame)
	}

	b.Run(r.Context())
	c.Close(websocket.StatusNormalClosure, "")
}

// ---- push ------------------------------------------------------------------

func (s *server) handleVAPIDKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"key": s.push.PublicKey()})
}

func (s *server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.FromRequest(r)
	if sess == nil {
		httpErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
		httpErr(w, http.StatusBadRequest, "invalid subscription")
		return
	}
	s.push.Subscribe(sess.UID, sub)
	writeJSON(w, map[string]any{"ok": true})
}

// ---- internal notify (websh-notify CLI) ------------------------------------

func (s *server) handleInternalNotify(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		httpErr(w, http.StatusForbidden, "forbidden")
		return
	}
	var req struct {
		User    string `json:"user"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !hmac.Equal([]byte(req.Token), []byte(s.notifyToken(req.User))) {
		httpErr(w, http.StatusForbidden, "bad token")
		return
	}
	u, err := user.Lookup(req.User)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "unknown user")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "终端需要你的关注"
	}
	s.attention(u.Uid, msg)
	writeJSON(w, map[string]any{"ok": true})
}

// attention routes an attention event: in-page if a foregrounded tab is live,
// else a Web Push.
func (s *server) attention(uid, msg string) {
	frame, _ := json.Marshal(map[string]any{"type": "attention", "message": msg})
	if s.presence.Notify(uid, frame) {
		payload, _ := json.Marshal(map[string]any{"title": "websh", "body": msg, "url": "/"})
		go s.push.Send(uid, payload)
	}
}

// notifyToken is the per-user token for websh-notify (HMAC of the username).
func (s *server) notifyToken(username string) string {
	mac := hmac.New(sha256.New, s.notifySecret)
	mac.Write([]byte(username))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ---- static ----------------------------------------------------------------

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/service-worker.js" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	s.files.ServeHTTP(w, r)
}

// staticHandler returns the web-UI file server: from dir if non-empty (dev),
// otherwise from the assets embedded in the binary.
func staticHandler(dir string) (http.Handler, string) {
	if dir != "" {
		return http.FileServer(http.Dir(dir)), dir
	}
	sub, err := fs.Sub(websh.StaticFS, "static")
	if err != nil {
		log.Fatalf("embedded assets: %v", err)
	}
	return http.FileServerFS(sub), "embedded"
}

// ---- helpers ---------------------------------------------------------------

func httpErr(w http.ResponseWriter, code int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"detail": detail})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackAddr(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func loginShell(username string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) >= 7 && parts[0] == username {
			return parts[6]
		}
	}
	return ""
}

func badShell(sh string) bool {
	switch filepath.Base(sh) {
	case "false", "nologin":
		return true
	}
	return false
}

// ---- secrets ---------------------------------------------------------------

type secrets struct {
	VAPIDPrivate string `json:"vapid_private"`
	VAPIDPublic  string `json:"vapid_public"`
	NotifySecret string `json:"notify_secret"`
}

func (s *secrets) notifySecretBytes() []byte {
	b, _ := base64.RawURLEncoding.DecodeString(s.NotifySecret)
	return b
}

func loadSecrets(path string) (*secrets, error) {
	if data, err := os.ReadFile(path); err == nil {
		var s secrets
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, err
		}
		return &s, nil
	}
	// generate
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, err
	}
	nb := make([]byte, 32)
	if _, err := rand.Read(nb); err != nil {
		return nil, err
	}
	s := &secrets{VAPIDPrivate: priv, VAPIDPublic: pub, NotifySecret: base64.RawURLEncoding.EncodeToString(nb)}
	data, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	log.Printf("generated VAPID + notify secrets at %s", path)
	return s, nil
}

// ---- login rate limiter ----------------------------------------------------

type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	rec    map[string]*failRec
}

type failRec struct {
	count int
	until time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, rec: make(map[string]*failRec)}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rec[key]
	if r == nil {
		return true
	}
	if time.Now().After(r.until) {
		delete(l.rec, key)
		return true
	}
	return r.count < l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rec[key]
	if r == nil || time.Now().After(r.until) {
		r = &failRec{}
		l.rec[key] = r
	}
	r.count++
	r.until = time.Now().Add(l.window)
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.rec, key)
	l.mu.Unlock()
}
