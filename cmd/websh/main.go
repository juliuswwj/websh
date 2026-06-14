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
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/coder/websocket"

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
		staticDir    = flag.String("static", "./static", "static assets directory")
		secretsPath  = flag.String("secrets", "/var/lib/websh/secrets.json", "VAPID/notify secrets file")
		pushPath     = flag.String("push-store", "/var/lib/websh/push_subs.json", "push subscription store")
		pamService   = flag.String("pam-service", "websh", "PAM service name")
		mailto       = flag.String("vapid-mailto", "mailto:admin@websh.local", "VAPID subscriber contact")
		secureCookie = flag.Bool("secure-cookie", false, "set Secure flag on the session cookie (enable behind HTTPS)")
		sessionTTL   = flag.Duration("session-ttl", 7*24*time.Hour, "web login session lifetime")
		idleTTL      = flag.Duration("idle-ttl", 72*time.Hour, "reclaim tmux sessions with no user input for this long")
	)
	_ = flag.CommandLine.Parse(args)

	if os.Geteuid() != 0 {
		log.Printf("WARNING: not running as root — only sessions for the current user (%d) will work, and PAM auth needs root", os.Getuid())
	}

	sec, err := loadSecrets(*secretsPath)
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}

	srv := &server{
		sessions:     auth.NewStore(*sessionTTL),
		presence:     presence.New(),
		push:         push.NewStore(*pushPath, sec.VAPIDPrivate, sec.VAPIDPublic, *mailto),
		files:        http.FileServer(http.Dir(*staticDir)),
		staticDir:    *staticDir,
		pamService:   *pamService,
		secureCookie: *secureCookie,
		notifySecret: sec.notifySecretBytes(),
		limiter:      newLimiter(5, 5*time.Minute),
	}
	notifyURL := "http://" + loopbackAddr(*bind) + "/internal/notify"
	srv.mgr = session.NewManager(*idleTTL, notifyURL, srv.notifyToken)

	stop := make(chan struct{})
	go srv.sessions.GC(stop)
	go srv.mgr.Janitor(stop)

	httpSrv := &http.Server{Addr: *bind, Handler: srv.routes()}

	go func() {
		log.Printf("websh listening on %s (static=%s)", *bind, *staticDir)
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
	mux.HandleFunc("GET /api/push/vapid-public-key", s.handleVAPIDKey)
	mux.HandleFunc("POST /api/push/subscribe", s.handleSubscribe)
	mux.HandleFunc("GET /ws/term/{id}", s.handleWSTerm)
	mux.HandleFunc("POST /internal/notify", s.handleInternalNotify)
	mux.HandleFunc("GET /", s.handleStatic)
	return mux
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
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusForbidden, "未配置 websh（缺少 ~/.config/websh.yaml）")
		return
	}
	if err != nil {
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusUnauthorized, "登录失败")
		return
	}
	if sh := loginShell(req.Username); badShell(sh) {
		httpErr(w, http.StatusForbidden, "该账号不允许登录")
		return
	}
	if err := auth.PAMAuthenticate(s.pamService, req.Username, req.Password); err != nil {
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if !auth.VerifyTOTP(cfg.OTPSecret, req.OTP) {
		s.limiter.fail(req.Username)
		httpErr(w, http.StatusUnauthorized, "验证码错误")
		return
	}

	s.limiter.reset(req.Username)
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

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.FromRequest(r)
	if sess == nil {
		httpErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	cfg, u, err := config.LoadForUser(sess.User)
	if err != nil {
		httpErr(w, http.StatusForbidden, "配置不可用")
		return
	}
	live := s.mgr.LiveSessions(u)
	out := make([]map[string]any, 0, len(cfg.Sessions))
	for _, sp := range cfg.Sessions {
		out = append(out, map[string]any{
			"id":   sp.ID,
			"name": sp.Name,
			"type": sp.Type,
			"host": sp.Host,
			"live": live[sp.ID],
		})
	}
	writeJSON(w, map[string]any{"sessions": out, "display_name": cfg.DisplayName, "user": sess.User})
}

// ---- terminal websocket ----------------------------------------------------

func (s *server) handleWSTerm(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.FromRequest(r)
	if sess == nil {
		httpErr(w, http.StatusUnauthorized, "未登录")
		return
	}
	cfg, u, err := config.LoadForUser(sess.User)
	if err != nil {
		httpErr(w, http.StatusForbidden, "配置不可用")
		return
	}
	id := r.PathValue("id")
	spec, ok := cfg.Find(id)
	if !ok {
		httpErr(w, http.StatusNotFound, "未知会话")
		return
	}

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	client, err := s.mgr.Spawn(spec, u, 80, 24)
	if err != nil {
		_ = c.Write(r.Context(), websocket.MessageBinary, []byte("\r\n\x1b[91m启动会话失败: "+err.Error()+"\x1b[0m\r\n"))
		c.Close(websocket.StatusInternalError, "spawn failed")
		return
	}
	defer client.Close()

	uid, tab := sess.UID, id
	b := bridge.New(c, client)
	conn := s.presence.Add(uid, tab, b.Send)
	defer s.presence.Remove(conn)
	b.OnPresence = func(state string) { s.presence.SetState(conn, state) }
	b.OnAttention = func() { s.attention(uid, tab, "终端需要你的关注") }

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
		Session string `json:"session"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !hmac.Equal([]byte(req.Token), []byte(s.notifyToken(req.Session))) {
		httpErr(w, http.StatusForbidden, "bad token")
		return
	}
	uid, slug, ok := session.ParseSessionName(req.Session)
	if !ok {
		httpErr(w, http.StatusBadRequest, "bad session")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "终端需要你的关注"
	}
	s.attention(uid, slug, msg)
	writeJSON(w, map[string]any{"ok": true})
}

// attention routes an attention event: in-page if a foregrounded tab is live,
// else a Web Push.
func (s *server) attention(uid, tab, msg string) {
	frame, _ := json.Marshal(map[string]any{"type": "attention", "tabId": tab, "message": msg})
	if s.presence.Notify(uid, tab, frame) {
		payload, _ := json.Marshal(map[string]any{"title": "websh", "body": msg, "tabId": tab, "url": "/"})
		go s.push.Send(uid, payload)
	}
}

func (s *server) notifyToken(sessionName string) string {
	mac := hmac.New(sha256.New, s.notifySecret)
	mac.Write([]byte(sessionName))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ---- static ----------------------------------------------------------------

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/service-worker.js" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	s.files.ServeHTTP(w, r)
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
