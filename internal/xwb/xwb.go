// Package xwb is a minimal client for the x-workbench ("xwb") upstream API,
// reverse-engineered from the reference gateway. It is used by the websh daemon
// (to resolve servers/credentials and create claude tabs) and by the xwb-proxy
// subcommand (to obtain a JWT for the terminal websocket).
//
// Auth is email+password: POST /api/auth/login returns a JWT, which REST calls
// carry as a Bearer token and websockets carry as a ?token= query param. The JWT
// is cached on disk (per user home) and re-fetched on expiry, so one login serves
// the daemon and every proxy.
package xwb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Creds is the x-workbench account + endpoint (from the websh.yaml xwb: section).
type Creds struct {
	Host     string // upstream host:port, e.g. 172.60.1.35:9630
	Email    string
	Password string
}

// Owner is the uid/gid to chown cache files to (when the daemon runs as root and
// writes into a user's home). Nil when running as the user already.
type Owner struct{ UID, GID int }

// Client talks to one x-workbench upstream on behalf of one user.
type Client struct {
	Creds Creds
	Home  string // user home dir; the token cache lives under ~/.cache/websh
	Owner *Owner // non-nil -> chown written cache files (daemon-as-root)

	HTTP *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New builds a client. homeDir is the user's home (for the token cache); owner is
// non-nil only when the caller runs as root and must chown cache files.
func New(creds Creds, homeDir string, owner *Owner) *Client {
	return &Client{
		Creds: creds,
		Home:  homeDir,
		Owner: owner,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) apiBase() string { return "http://" + c.Creds.Host + "/api" }

// Origin is the value sent as the Origin header on websocket connects; the
// upstream validates it.
func (c *Client) Origin() string { return "http://" + c.Creds.Host }

func (c *Client) tokenPath() string {
	return filepath.Join(c.Home, ".cache", "websh", "xwb-token.json")
}

// ---- token ----------------------------------------------------------------

type tokenCache struct {
	Token string `json:"token"`
	Exp   int64  `json:"exp"` // unix seconds
}

// Token returns a valid cached JWT, logging in if the cache is missing/expired.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expiry.Add(-60*time.Second)) {
		return c.token, nil
	}
	// Try the on-disk cache (another process may have refreshed it).
	if tc, err := c.readCache(); err == nil && tc.Token != "" {
		exp := time.Unix(tc.Exp, 0)
		if time.Now().Before(exp.Add(-60 * time.Second)) {
			c.token, c.expiry = tc.Token, exp
			return c.token, nil
		}
	}
	return c.loginLocked(ctx)
}

// ForceLogin discards any cached token and logs in afresh. Used after the
// websocket is closed with an auth failure (4401).
func (c *Client) ForceLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expiry = "", time.Time{}
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"email": c.Creds.Email, "password": c.Creds.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("xwb login: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("xwb login failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return "", errors.New("xwb login: upstream returned no token")
	}
	c.token = out.Token
	c.expiry = jwtExpiry(out.Token)
	c.writeCache(tokenCache{Token: c.token, Exp: c.expiry.Unix()})
	return c.token, nil
}

// jwtExpiry extracts the exp claim from a JWT (no signature check). Falls back to
// a short window so a token with no/garbled exp is still cached briefly.
func jwtExpiry(token string) time.Time {
	fallback := time.Now().Add(30 * time.Minute)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return fallback
	}
	return time.Unix(claims.Exp, 0)
}

func (c *Client) readCache() (tokenCache, error) {
	var tc tokenCache
	data, err := os.ReadFile(c.tokenPath())
	if err != nil {
		return tc, err
	}
	err = json.Unmarshal(data, &tc)
	return tc, err
}

func (c *Client) writeCache(tc tokenCache) {
	dir := filepath.Dir(c.tokenPath())
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	data, _ := json.Marshal(tc)
	tmp := c.tokenPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if os.Rename(tmp, c.tokenPath()) != nil {
		_ = os.Remove(tmp)
		return
	}
	if c.Owner != nil {
		// Chown the cache dir tree so the user's proxy can read/refresh it.
		_ = os.Chown(filepath.Join(c.Home, ".cache"), c.Owner.UID, c.Owner.GID)
		_ = os.Chown(dir, c.Owner.UID, c.Owner.GID)
		_ = os.Chown(c.tokenPath(), c.Owner.UID, c.Owner.GID)
	}
}

// ---- REST -----------------------------------------------------------------

func (c *Client) get(ctx context.Context, path string, out any) error {
	token, err := c.Token(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("xwb GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("xwb GET %s (HTTP %d): %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// flexInt accepts a JSON number or a quoted number (upstream is inconsistent).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// asList unwraps the array/{items}/{servers} shapes the upstream uses.
func asList(raw json.RawMessage) []json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(t, &arr) == nil {
			return arr
		}
	}
	var wrap struct {
		Items    []json.RawMessage `json:"items"`
		Servers  []json.RawMessage `json:"servers"`
		Sessions []json.RawMessage `json:"sessions"`
	}
	if json.Unmarshal(t, &wrap) == nil {
		switch {
		case wrap.Items != nil:
			return wrap.Items
		case wrap.Servers != nil:
			return wrap.Servers
		case wrap.Sessions != nil:
			return wrap.Sessions
		}
	}
	return nil
}

// Server is one x-workbench server. The upstream field for the address is
// unverified, so several candidates are decoded and matched leniently.
type Server struct {
	ID       flexInt `json:"id"`
	Name     string  `json:"name"`
	IP       string  `json:"ip"`
	Host     string  `json:"host"`
	Hostname string  `json:"hostname"`
	Address  string  `json:"address"`
}

func (s Server) addrs() []string {
	return []string{s.IP, s.Host, s.Hostname, s.Address, s.Name}
}

// ListServers returns the servers visible to the account.
func (c *Client) ListServers(ctx context.Context) ([]Server, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/wb/servers", &raw); err != nil {
		return nil, err
	}
	var out []Server
	for _, item := range asList(raw) {
		var s Server
		if json.Unmarshal(item, &s) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// ResolveServer maps a configured server IP to its upstream server_id.
func (c *Client) ResolveServer(ctx context.Context, ip string) (int, error) {
	servers, err := c.ListServers(ctx)
	if err != nil {
		return 0, err
	}
	want := strings.TrimSpace(strings.ToLower(ip))
	for _, s := range servers {
		for _, a := range s.addrs() {
			a = strings.TrimSpace(strings.ToLower(a))
			if a == "" {
				continue
			}
			// Match exact, or host portion of a "host:port" address.
			if a == want || strings.Split(a, ":")[0] == want {
				return int(s.ID), nil
			}
		}
	}
	return 0, fmt.Errorf("xwb: no server matches ip %q", ip)
}

type credential struct {
	ID         flexInt `json:"id"`
	IsDefault  bool    `json:"is_default"`
	IsVerified bool    `json:"is_verified"`
}

// ResolveCredential returns the credential_id to use for a server. If override is
// non-zero it is used as-is; otherwise the default/verified/first credential wins
// (mirroring the reference frontend).
func (c *Client) ResolveCredential(ctx context.Context, serverID, override int) (int, error) {
	if override != 0 {
		return override, nil
	}
	var raw json.RawMessage
	if err := c.get(ctx, fmt.Sprintf("/wb/servers/%d/credentials", serverID), &raw); err != nil {
		return 0, err
	}
	var creds []credential
	for _, item := range asList(raw) {
		var cr credential
		if json.Unmarshal(item, &cr) == nil {
			creds = append(creds, cr)
		}
	}
	if len(creds) == 0 {
		return 0, fmt.Errorf("xwb: server %d has no credentials", serverID)
	}
	for _, cr := range creds {
		if cr.IsDefault {
			return int(cr.ID), nil
		}
	}
	for _, cr := range creds {
		if cr.IsVerified {
			return int(cr.ID), nil
		}
	}
	return int(creds[0].ID), nil
}

// StartClaudeTab creates a claude session upstream and returns its tab_id. Unlike
// bash tabs, claude tabs are persisted upstream (re-listable via CicadaSessions).
func (c *Client) StartClaudeTab(ctx context.Context, serverID, credentialID int) (string, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return "", err
	}
	payload := map[string]any{"server_id": serverID}
	if credentialID != 0 {
		payload["credential_id"] = credentialID
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/workbench/claude-tab/start", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("xwb claude-tab/start: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("xwb claude-tab/start (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		TabID string `json:"tab_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.TabID == "" {
		return "", errors.New("xwb claude-tab/start: no tab_id returned")
	}
	return out.TabID, nil
}

// CicadaSession is a claude session as listed by the upstream cicada service.
type CicadaSession struct {
	ServerID flexInt `json:"server_id"`
	TabID    string  `json:"tab_id"`
	Name     string  `json:"name"`
	CWD      string  `json:"cwd"`
	State    string  `json:"state"`
	Model    string  `json:"model"`
}

// CicadaSessions lists the account's claude sessions (all servers).
func (c *Client) CicadaSessions(ctx context.Context) ([]CicadaSession, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/wb/cicada/sessions?group=state", &raw); err != nil {
		return nil, err
	}
	var out []CicadaSession
	for _, item := range asList(raw) {
		var s CicadaSession
		if json.Unmarshal(item, &s) == nil && s.TabID != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// DeleteClaude removes a claude session upstream (best-effort).
func (c *Client) DeleteClaude(ctx context.Context, tabID string) error {
	token, err := c.Token(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiBase()+"/wb/cicada/sessions/"+url.PathEscape(tabID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("xwb delete claude (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// ---- websocket URLs -------------------------------------------------------

// TermWSURL builds the bash terminal websocket URL for a server + tab.
func (c *Client) TermWSURL(serverID, credentialID int, tabID, token string) string {
	q := url.Values{}
	q.Set("token", token)
	if credentialID != 0 {
		q.Set("credential_id", strconv.Itoa(credentialID))
	}
	q.Set("tab_id", tabID)
	return fmt.Sprintf("ws://%s/api/wb/term/%d?%s", c.Creds.Host, serverID, q.Encode())
}

// ClaudeWSURL builds the claude stream websocket URL for a tab.
func (c *Client) ClaudeWSURL(tabID, token string) string {
	q := url.Values{}
	q.Set("tab_id", tabID)
	q.Set("token", token)
	return fmt.Sprintf("ws://%s/api/wb/claude-tab/stream?%s", c.Creds.Host, q.Encode())
}
