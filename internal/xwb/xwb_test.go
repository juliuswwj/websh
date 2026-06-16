package xwb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestClient() *Client {
	return New(Creds{Host: "172.60.1.35:9630", Email: "a@b.c", Password: "x"}, "/tmp", nil)
}

func TestTermWSURL(t *testing.T) {
	c := newTestClient()
	raw := c.TermWSURL(5, 219, "mob-bash-abc", "TOKEN")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "ws" || u.Host != "172.60.1.35:9630" || u.Path != "/api/wb/term/5" {
		t.Fatalf("bad term url base: %s", raw)
	}
	q := u.Query()
	if q.Get("token") != "TOKEN" || q.Get("credential_id") != "219" || q.Get("tab_id") != "mob-bash-abc" {
		t.Fatalf("bad term url query: %s", raw)
	}
}

func TestTermWSURLNoCredential(t *testing.T) {
	c := newTestClient()
	raw := c.TermWSURL(5, 0, "tab", "T")
	if strings.Contains(raw, "credential_id") {
		t.Fatalf("credential_id should be omitted when zero: %s", raw)
	}
}

func TestClaudeWSURL(t *testing.T) {
	c := newTestClient()
	raw := c.ClaudeWSURL("tab-1", "T")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/api/wb/claude-tab/stream" || u.Query().Get("tab_id") != "tab-1" || u.Query().Get("token") != "T" {
		t.Fatalf("bad claude url: %s", raw)
	}
}

func TestJWTExpiry(t *testing.T) {
	want := time.Now().Add(2 * time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"96","exp":` + itoa(want) + `}`))
	tok := "h." + payload + ".s"
	got := jwtExpiry(tok).Unix()
	if got != want {
		t.Fatalf("jwtExpiry = %d, want %d", got, want)
	}
	// Garbled token -> non-zero fallback in the future.
	if !jwtExpiry("not-a-jwt").After(time.Now()) {
		t.Fatal("fallback expiry should be in the future")
	}
}

func TestFlexInt(t *testing.T) {
	var s struct {
		ID flexInt `json:"id"`
	}
	if json.Unmarshal([]byte(`{"id":42}`), &s) != nil || s.ID != 42 {
		t.Fatalf("number id failed: %d", s.ID)
	}
	if json.Unmarshal([]byte(`{"id":"7"}`), &s) != nil || s.ID != 7 {
		t.Fatalf("string id failed: %d", s.ID)
	}
}

func TestAsListShapes(t *testing.T) {
	cases := []string{`[{"id":1}]`, `{"servers":[{"id":1}]}`, `{"items":[{"id":1}]}`}
	for _, c := range cases {
		if got := asList([]byte(c)); len(got) != 1 {
			t.Fatalf("asList(%s) -> %d items", c, len(got))
		}
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// makeJWT builds an unsigned JWT whose payload carries the given exp.
func makeJWT(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"96","exp":` + itoa(exp) + `}`))
	return "h." + payload + ".s"
}

func TestLoginResolveAndCache(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	jwt := makeJWT(exp)
	var loginCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		var body struct{ Email, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Email != "a@b.c" || body.Password != "pw" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"token":"` + jwt + `","user":{"id":96}}`))
	})
	mux.HandleFunc("/api/wb/servers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+jwt {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"servers":[{"id":5,"name":"gpu","ip":"10.0.0.5"},{"id":6,"host":"10.0.0.6:22"}]}`))
	})
	mux.HandleFunc("/api/wb/servers/5/credentials", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":7},{"id":219,"is_default":true}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	home := t.TempDir()
	c := New(Creds{Host: strings.TrimPrefix(srv.URL, "http://"), Email: "a@b.c", Password: "pw"}, home, nil)
	ctx := context.Background()

	tok, err := c.Token(ctx)
	if err != nil || tok != jwt {
		t.Fatalf("Token = %q, err=%v", tok, err)
	}
	// Second call is served from the in-memory cache (no extra login).
	if _, err := c.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if loginCount != 1 {
		t.Fatalf("loginCount = %d, want 1 (cached)", loginCount)
	}
	// Token persisted to disk, 0600.
	fi, err := os.Stat(c.tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token cache mode = %v, want 0600", fi.Mode().Perm())
	}

	// IP resolves to server_id (exact match, and host:port match).
	if id, err := c.ResolveServer(ctx, "10.0.0.5"); err != nil || id != 5 {
		t.Fatalf("ResolveServer(10.0.0.5) = %d, err=%v", id, err)
	}
	if id, err := c.ResolveServer(ctx, "10.0.0.6"); err != nil || id != 6 {
		t.Fatalf("ResolveServer(10.0.0.6) = %d, err=%v", id, err)
	}
	if _, err := c.ResolveServer(ctx, "10.0.0.9"); err == nil {
		t.Fatal("ResolveServer(unknown) should error")
	}

	// Default credential wins; explicit override is returned as-is.
	if id, err := c.ResolveCredential(ctx, 5, 0); err != nil || id != 219 {
		t.Fatalf("ResolveCredential = %d, err=%v", id, err)
	}
	if id, _ := c.ResolveCredential(ctx, 5, 42); id != 42 {
		t.Fatalf("ResolveCredential override = %d, want 42", id)
	}
}

func TestTokenCacheSharedAcrossClients(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	jwt := makeJWT(exp)
	var loginCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		_, _ = w.Write([]byte(`{"token":"` + jwt + `"}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	c1 := New(Creds{Host: host, Email: "a@b.c", Password: "pw"}, home, nil)
	if _, err := c1.Token(ctx); err != nil {
		t.Fatal(err)
	}
	// A fresh client (e.g. the proxy process) reuses the on-disk cache.
	c2 := New(Creds{Host: host, Email: "a@b.c", Password: "pw"}, home, nil)
	if tok, err := c2.Token(ctx); err != nil || tok != jwt {
		t.Fatalf("c2.Token = %q, err=%v", tok, err)
	}
	if loginCount != 1 {
		t.Fatalf("loginCount = %d, want 1 (cache shared on disk)", loginCount)
	}
}
