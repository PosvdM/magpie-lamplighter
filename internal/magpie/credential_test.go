package magpie

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

var testNow = time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)

func useClock(t *testing.T) {
	t.Helper()
	now = func() time.Time { return testNow }
	t.Cleanup(func() { now = time.Now })
}

// writeFiles writes files into dir, creating their folders.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// jwt is an unsigned JWT with claims.
func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// loginsFile is a logins.json in the shape of savedLogin in Magpie's
// internal/provider/logins.go: a Codex and a Claude account with ids of
// their own, a Claude account added with a setup-token whose id is made of
// its name, and two agy-cli accounts.
func loginsFile(codexAccess, claudeAuth string) string {
	return mustJSON([]map[string]any{
		{"id": "1a2b3c4d5e6f7a8b", "agent": "codex", "user": "a@example.com · Team", "plan": "team", "seen": "2026-11-21T07:00:00Z",
			"auth": map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
				"id_token":      jwt(map[string]any{"email": "a@example.com", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1"}}),
				"access_token":  codexAccess,
				"refresh_token": "codex-refresh",
			}}},
		{"id": "0f0e0d0c0b0a0908", "agent": "claude", "user": "b@example.com", "seen": "2026-11-21T07:00:00Z", "auth": json.RawMessage(claudeAuth)},
		{"agent": "claude", "user": "Setup token …x1y2", "seen": "2026-11-21T07:00:00Z", "auth": map[string]any{"setupToken": "sk-ant-oat01-setup"}},
		{"id": "abcdabcdabcdabcd", "agent": "plugin:agy-cli", "user": "c@example.com", "home": "agy-cli#k3x9a2", "on": true, "auth": nil},
		{"id": "1234123412341234", "agent": "plugin:agy-cli", "user": "d@example.com", "home": "agy-cli", "on": true, "auth": nil},
	})
}

func claudeAuth(access, refresh string, expires time.Time) string {
	return mustJSON(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refresh, "expiresAt": expires.UnixMilli(),
		"scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max",
	}})
}

func TestCodexCredential(t *testing.T) {
	useClock(t)
	h := &Host{ConfigDir: t.TempDir()}
	access := jwt(map[string]any{"exp": float64(testNow.Add(time.Hour).Unix())})
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": loginsFile(access, claudeAuth("x", "y", testNow))})
	got, err := h.Credential(context.Background(), host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != access || got.AccountID != "acct-1" || got.Expired || got.Fingerprint != fingerprint("codex-refresh") || got.ID != "codex/1a2b3c4d5e6f7a8b" {
		t.Fatalf("credential = %+v", got)
	}
	// Lamplighter never refreshes a Codex token: one past its exp is Expired.
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": loginsFile(jwt(map[string]any{"exp": float64(testNow.Unix())}), "{}")})
	if got, err := h.Credential(context.Background(), host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"}); err != nil || !got.Expired || !got.RenewedByRefresh {
		t.Fatalf("expired token: %+v, %v", got, err)
	}
}

func TestClaudeCredential(t *testing.T) {
	useClock(t)
	h := &Host{ConfigDir: t.TempDir()}
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": loginsFile("", claudeAuth("old-access", "rt-1", testNow.Add(-time.Minute)))})
	account := host.Account{Pin: "claude/0f0e0d0c0b0a0908", Provider: "claude"}
	got, err := h.Credential(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "old-access" || !got.Expired || !got.RenewedByRefresh || got.Fingerprint != fingerprint("rt-1") {
		t.Fatalf("credential = %+v", got)
	}
	// Claude Code keeps a saved account's sign-in in claude-accounts/<hash>,
	// where it refreshes it; a later one there is used.
	dir := "claude-accounts/" + "e8f39b3e1382367d" // sha256("b@example.com")[:16], as claudeAccountDir names it
	writeFiles(t, h.ConfigDir, map[string]string{dir + "/.credentials.json": claudeAuth("new-access", "rt-2", testNow.Add(time.Hour))})
	got, err = h.Credential(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.Expired || got.RenewedByRefresh || got.Fingerprint != fingerprint("rt-2") {
		t.Fatalf("credential with Claude Code's own copy = %+v (is %s the account's folder?)", got, dir)
	}
	// An account added with a setup-token reads Magpie's kept quota.
	got, err = h.Credential(context.Background(), host.Account{Pin: "claude/" + nameID("claude", "Setup token …x1y2"), Provider: "claude"})
	if err != nil || !got.Expired || got.RenewedByRefresh || got.AccessToken != "" || got.Fingerprint != fingerprint("sk-ant-oat01-setup") {
		t.Fatalf("setup-token account: %+v, %v", got, err)
	}
}

// agyFixture sets up the agy-cli plugin's accounts: "c@example.com" with a
// home of its own, "d@example.com" agy's own.
func TestCredentialFormat(t *testing.T) {
	useClock(t)
	cases := []struct {
		name    string
		files   map[string]string
		account host.Account
	}{
		{"logins.json not a list", map[string]string{"logins.json": `{"accounts":[]}`}, host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"}},
		{"Codex auth without tokens", map[string]string{"logins.json": `[{"id":"x1","agent":"codex","user":"a","auth":{"auth_mode":"chatgpt"}}]`}, host.Account{Pin: "codex/x1", Provider: "codex"}},
		{"Claude auth of another shape", map[string]string{"logins.json": `[{"id":"x1","agent":"claude","user":"a","auth":{"oauth":{}}}]`}, host.Account{Pin: "claude/x1", Provider: "claude"}},
		{"logins.json entry null", map[string]string{"logins.json": `[null]`}, host.Account{Pin: "codex/x1", Provider: "codex"}},
		{"Codex tokens empty", map[string]string{"logins.json": `[{"id":"x1","agent":"codex","user":"a","auth":{"auth_mode":"chatgpt","tokens":{}}}]`}, host.Account{Pin: "codex/x1", Provider: "codex"}},
		{"Claude claudeAiOauth empty", map[string]string{"logins.json": `[{"id":"x1","agent":"claude","user":"a","auth":{"claudeAiOauth":{}}}]`}, host.Account{Pin: "claude/x1", Provider: "claude"}},
		{"Claude Code's file not JSON", map[string]string{
			"logins.json": loginsFile("", claudeAuth("old-access", "rt-1", testNow.Add(time.Hour))),
			"claude-accounts/e8f39b3e1382367d/.credentials.json": `{"claudeAiOauth":`,
		}, host.Account{Pin: "claude/0f0e0d0c0b0a0908", Provider: "claude"}},
		{"Claude Code's file not an object", map[string]string{
			"logins.json": loginsFile("", claudeAuth("old-access", "rt-1", testNow.Add(time.Hour))),
			"claude-accounts/e8f39b3e1382367d/.credentials.json": `[]`,
		}, host.Account{Pin: "claude/0f0e0d0c0b0a0908", Provider: "claude"}},
		{"providers.json broken", map[string]string{
			"logins.json":    loginsFile(jwt(map[string]any{}), "{}"),
			"providers.json": `{"providers":{}}`,
		}, host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"}},
	}
	for _, c := range cases {
		h := &Host{ConfigDir: t.TempDir()}
		writeFiles(t, h.ConfigDir, c.files)
		if _, err := h.Credential(context.Background(), c.account); !errors.Is(err, host.ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", c.name, err)
		}
	}
	// An account that is not in logins.json is an error, not a format one.
	h := &Host{ConfigDir: t.TempDir()}
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": `[]`})
	if _, err := h.Credential(context.Background(), host.Account{Pin: "codex/nope", Provider: "codex"}); err == nil || errors.Is(err, host.ErrFormat) {
		t.Errorf("unknown account: err = %v", err)
	}
}

// Known states without a usable credential are told apart from formats
// Lamplighter does not know and from optional files that are missing.
func TestCredentialKnownStates(t *testing.T) {
	useClock(t)
	ctx := context.Background()
	// Magpie writes "null" for a list without accounts.
	h := &Host{ConfigDir: t.TempDir()}
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": "null\n"})
	if _, err := h.Credential(ctx, host.Account{Pin: "codex/x1", Provider: "codex"}); err == nil || errors.Is(err, host.ErrFormat) {
		t.Errorf("logins.json null: err = %v, want no account", err)
	}
	// A Codex sign-in Magpie imported from a refresh token alone has no
	// access token: Expired, so the engine reads the kept quota.
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": `[{"id":"x1","agent":"codex","user":"a","auth":{"auth_mode":"chatgpt","tokens":{"refresh_token":"rt"}}}]`})
	if got, err := h.Credential(ctx, host.Account{Pin: "codex/x1", Provider: "codex"}); err != nil || !got.Expired || got.Fingerprint != fingerprint("rt") {
		t.Errorf("Codex refresh token only: %+v, %v", got, err)
	}
	// Claude Code's own file without claudeAiOauth is a signed-out one:
	// the copy in logins.json is used.
	account := host.Account{Pin: "claude/0f0e0d0c0b0a0908", Provider: "claude"}
	writeFiles(t, h.ConfigDir, map[string]string{
		"logins.json": loginsFile("", claudeAuth("old-access", "rt-1", testNow.Add(time.Hour))),
		"claude-accounts/e8f39b3e1382367d/.credentials.json": `{"mcpOAuth":{}}`,
	})
	if got, err := h.Credential(ctx, account); err != nil || got.AccessToken != "old-access" {
		t.Errorf("signed-out Claude Code file: %+v, %v", got, err)
	}
	// A file there that cannot be read is an error of its own.
	dir := filepath.Join(h.ConfigDir, "claude-accounts/e8f39b3e1382367d/.credentials.json")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Credential(ctx, account); err == nil || errors.Is(err, host.ErrFormat) {
		t.Errorf("unreadable Claude Code file: err = %v", err)
	}
}

func TestProxyFor(t *testing.T) {
	h := &Host{ConfigDir: t.TempDir()}
	writeFiles(t, h.ConfigDir, map[string]string{
		"providers.json": mustJSON(map[string]any{"providers": []any{
			map[string]any{"id": "codex", "proxy": "127.0.0.1:7890", "accountProxies": map[string]string{"a@example.com · team": "direct"}},
			map[string]any{"id": "claude", "accountProxies": map[string]string{"b@example.com": "@px01"}},
			map[string]any{"id": "agy-cli", "proxy": "@pxssh"},
			map[string]any{"id": "kimi", "proxy": "@gone"},
		}}),
		"settings.json": mustJSON(map[string]any{"proxy": "socks5h://10.0.0.1:1080", "proxies": []any{
			map[string]any{"id": "px01", "name": "Tokyo", "url": "http://proxy.example:3128", "user": "u", "pass": "p@ss"},
			map[string]any{"id": "pxssh", "name": "VPS", "url": "ssh://vps.example:22", "user": "root"},
		}}),
	})
	for _, c := range []struct{ provider, user, want string }{
		{"codex", "A@example.com · Team", "direct"},
		{"codex", "other@example.com", "http://127.0.0.1:7890"},
		{"claude", "b@example.com", "http://u:p%40ss@proxy.example:3128"},
		{"claude", "other@example.com", "socks5h://10.0.0.1:1080"},
		{"grok", "x", "socks5h://10.0.0.1:1080"},
	} {
		if got, err := h.proxyFor(c.provider, c.user); err != nil || got != c.want {
			t.Errorf("proxyFor(%s, %s) = %q, %v; want %q", c.provider, c.user, got, err, c.want)
		}
	}
	for _, provider := range []string{"agy-cli", "kimi"} {
		if _, err := h.proxyFor(provider, "x"); err == nil {
			t.Errorf("proxyFor(%s) succeeded", provider)
		}
	}
	// Without a proxy in Magpie's files, the environment's: egress then
	// takes HTTPS_PROXY with NO_PROXY, which the npm shell maps from the
	// MAGPIE_ variables. The plugin host's proxy is not an account's own.
	h = &Host{ConfigDir: t.TempDir()}
	t.Setenv("MAGPIE_HTTPS_PROXY", "http://127.0.0.1:61234")
	t.Setenv("MAGPIE_ALL_PROXY", "http://127.0.0.1:61235")
	t.Setenv("MAGPIE_HTTP_PROXY", "http://127.0.0.1:61236")
	if got, err := h.proxyFor("codex", "a"); err != nil || got != "" {
		t.Errorf("host proxy only: %q, %v; want the environment's", got, err)
	}
	writeFiles(t, h.ConfigDir, map[string]string{"settings.json": `{"proxy":"direct"}`})
	if got, err := h.proxyFor("codex", "a"); err != nil || got != "direct" {
		t.Errorf("global direct: %q, %v", got, err)
	}
}

// Nothing of an agy-cli account is read from Magpie's files: its quota
// comes from Magpie's quota cards alone.
func TestAgyCredentialIsNotRead(t *testing.T) {
	h := &Host{ConfigDir: t.TempDir()}
	writeFiles(t, h.ConfigDir, map[string]string{
		"logins.json":      `[{"agent":"plugin:agy-cli","user":"c@example.com","home":"agy-cli"}]`,
		"plugin-auth.json": `{"agy-cli":{"type":"api","key":"agy","metadata":{"email":"c@example.com"}}}`,
	})
	pin := "agy-cli/" + nameID("agy-cli", "c@example.com")
	if got, err := h.Credential(context.Background(), host.Account{Pin: pin, Provider: "agy-cli"}); err == nil || got.AccessToken != "" {
		t.Fatalf("agy-cli credential: %+v, %v", got, err)
	}
}
