package magpie

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
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
	if got, err := h.Credential(context.Background(), host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"}); err != nil || !got.Expired {
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
	if got.AccessToken != "old-access" || !got.Expired || got.Fingerprint != fingerprint("rt-1") {
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
	if got.AccessToken != "new-access" || got.Expired || got.Fingerprint != fingerprint("rt-2") {
		t.Fatalf("credential with Claude Code's own copy = %+v (is %s the account's folder?)", got, dir)
	}
	// An account added with a setup-token reads Magpie's kept quota.
	got, err = h.Credential(context.Background(), host.Account{Pin: "claude/" + nameID("claude", "Setup token …x1y2"), Provider: "claude"})
	if err != nil || !got.Expired || got.AccessToken != "" || got.Fingerprint != fingerprint("sk-ant-oat01-setup") {
		t.Fatalf("setup-token account: %+v, %v", got, err)
	}
}

// agyFixture sets up the agy-cli plugin's accounts: "c@example.com" with a
// home of its own, "d@example.com" agy's own.
func agyFixture(t *testing.T, token string) (*Host, string) {
	t.Helper()
	useClock(t)
	h := &Host{ConfigDir: t.TempDir()}
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", home)
	writeFiles(t, h.ConfigDir, map[string]string{
		"logins.json": loginsFile("", "{}"),
		"plugin-auth.json": mustJSON(map[string]any{
			"agy-cli":        map[string]any{"type": "api", "key": "agy", "metadata": map[string]any{"email": "d@example.com"}},
			"agy-cli#k3x9a2": map[string]any{"type": "api", "key": "agy:0123456789abcdef", "metadata": map[string]any{"email": "c@example.com", "agyHome": "0123456789abcdef"}},
		}),
	})
	writeFiles(t, state, map[string]string{"magpie-agy-bridge/accounts/0123456789abcdef/.gemini/antigravity-cli/antigravity-oauth-token": token})
	return h, home
}

func agyToken(access, refresh string, expiry time.Time) string {
	return mustJSON(map[string]any{"token": map[string]any{
		"access_token": access, "token_type": "Bearer", "refresh_token": refresh, "expiry": expiry.Format(time.RFC3339Nano),
	}, "auth_method": "consumer", "id_token_email": "c@example.com"})
}

// googleFake answers the token renewals.
type googleFake struct {
	status int
	body   string
	calls  []quota.Request
	proxy  []string
}

func (g *googleFake) Do(_ context.Context, proxy string, req quota.Request) (quota.Response, error) {
	g.calls = append(g.calls, req)
	g.proxy = append(g.proxy, proxy)
	return quota.Response{Status: g.status, Body: []byte(g.body)}, nil
}

func useGoogle(t *testing.T, g *googleFake) {
	t.Helper()
	was := googleDoer
	googleDoer = g
	t.Cleanup(func() { googleDoer = was })
}

var agyAccount = host.Account{Pin: "agy-cli/" + nameID("agy-cli", "c@example.com"), Provider: "agy-cli"}

func TestAgyCredential(t *testing.T) {
	h, _ := agyFixture(t, agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour)))
	g := &googleFake{status: 500}
	useGoogle(t, g)
	got, err := h.Credential(context.Background(), agyAccount)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "ya29.live" || got.ProjectID != consumerProject || got.Expired || got.Fingerprint != fingerprint("1//refresh") || len(g.calls) != 0 {
		t.Fatalf("credential = %+v, renewals %d", got, len(g.calls))
	}
}

func TestAgyRenewsInMemory(t *testing.T) {
	token := agyToken("ya29.old", "1//refresh", testNow.Add(time.Minute))
	h, _ := agyFixture(t, "go-keyring-base64:"+base64.StdEncoding.EncodeToString([]byte(token)))
	writeFiles(t, h.ConfigDir, map[string]string{"settings.json": `{"proxy":"socks5://127.0.0.1:1080"}`})
	g := &googleFake{status: 200, body: `{"access_token":"ya29.new","expires_in":3599,"token_type":"Bearer"}`}
	useGoogle(t, g)
	for range 2 {
		got, err := h.Credential(context.Background(), agyAccount)
		if err != nil {
			t.Fatal(err)
		}
		if got.AccessToken != "ya29.new" || got.ProxyURL != "socks5://127.0.0.1:1080" {
			t.Fatalf("credential = %+v", got)
		}
	}
	if len(g.calls) != 1 {
		t.Fatalf("renewed %d times, want once", len(g.calls))
	}
	form, _ := url.ParseQuery(string(g.calls[0].Body))
	if g.calls[0].URL != googleTokenURL || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "1//refresh" || form.Get("client_id") != googleClientID || g.proxy[0] != "socks5://127.0.0.1:1080" {
		t.Fatalf("renewal = %+v via %q", g.calls[0], g.proxy[0])
	}
	// The token file is agy's: it is never written.
	b, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "magpie-agy-bridge/accounts/0123456789abcdef/.gemini/antigravity-cli/antigravity-oauth-token"))
	if !strings.HasPrefix(string(b), "go-keyring-base64:") {
		t.Fatal("the token file changed")
	}
}

func TestAgyRefused(t *testing.T) {
	h, _ := agyFixture(t, agyToken("", "1//revoked", time.Time{}))
	useGoogle(t, &googleFake{status: 400, body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`})
	if _, err := h.Credential(context.Background(), agyAccount); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err = %v", err)
	}
	// Without a refresh token an ended access token is Expired.
	h, _ = agyFixture(t, agyToken("ya29.old", "", testNow.Add(-time.Minute)))
	if got, err := h.Credential(context.Background(), agyAccount); err != nil || !got.Expired {
		t.Fatalf("no refresh token: %+v, %v", got, err)
	}
	// Go's zero time is a token that does not end.
	h, _ = agyFixture(t, agyToken("ya29.forever", "", time.Time{}))
	if got, err := h.Credential(context.Background(), agyAccount); err != nil || got.Expired || got.AccessToken != "ya29.forever" {
		t.Fatalf("zero expiry: %+v, %v", got, err)
	}
}

// agy's own account is in the keyring on macOS and Windows, and in
// ~/.gemini on Linux without one.
func TestAgyOwnAccount(t *testing.T) {
	h, home := agyFixture(t, "{}")
	own := host.Account{Pin: "agy-cli/" + nameID("agy-cli", "d@example.com"), Provider: "agy-cli"}
	defer func(was string) { goos = was }(goos)
	goos = "darwin"
	if _, err := h.Credential(context.Background(), own); !errors.Is(err, errAgyKeyring) {
		t.Fatalf("darwin: err = %v, want errAgyKeyring", err)
	}
	goos = "linux"
	writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agyToken("ya29.own", "1//own", testNow.Add(time.Hour))})
	got, err := h.Credential(context.Background(), own)
	if err != nil || got.AccessToken != "ya29.own" {
		t.Fatalf("linux: %+v, %v", got, err)
	}
}

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
		{"agyHome not a home id", map[string]string{
			"logins.json":      loginsFile("", "{}"),
			"plugin-auth.json": `{"agy-cli#k3x9a2":{"type":"api","metadata":{"agyHome":"../../etc"}}}`,
		}, agyAccount},
		{"plugin-auth.json not an object", map[string]string{"logins.json": loginsFile("", "{}"), "plugin-auth.json": `[]`}, agyAccount},
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
		{"agy-cli login without home", map[string]string{
			"logins.json":      `[{"agent":"plugin:agy-cli","user":"c@example.com"}]`,
			"plugin-auth.json": `{}`,
		}, agyAccount},
		{"agy-cli entry null", map[string]string{"logins.json": loginsFile("", "{}"), "plugin-auth.json": `{"agy-cli#k3x9a2":null}`}, agyAccount},
		{"agy-cli entry not api", map[string]string{"logins.json": loginsFile("", "{}"), "plugin-auth.json": `{"agy-cli#k3x9a2":{"type":"oauth","key":"agy"}}`}, agyAccount},
		{"agyHome that its key does not name", map[string]string{
			"logins.json":      loginsFile("", "{}"),
			"plugin-auth.json": `{"agy-cli#k3x9a2":{"type":"api","key":"agy","metadata":{"agyHome":"0123456789abcdef"}}}`,
		}, agyAccount},
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
	for name, token := range map[string]string{
		"token file without token":   `{"token_type":"Bearer"}`,
		"expiry that does not parse": strings.Replace(agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour)), testNow.Add(time.Hour).Format(time.RFC3339Nano), "tomorrow", 1),
		"token without expiry":       `{"token":{"access_token":"ya29.live","refresh_token":"1//refresh"},"auth_method":"consumer"}`,
	} {
		h, _ := agyFixture(t, token)
		useGoogle(t, &googleFake{status: 500})
		if got, err := h.Credential(context.Background(), agyAccount); !errors.Is(err, host.ErrFormat) {
			t.Errorf("%s: %+v, err = %v, want ErrFormat", name, got, err)
		}
	}
	// An account that is not in logins.json is an error, not a format one.
	h := &Host{ConfigDir: t.TempDir()}
	writeFiles(t, h.ConfigDir, map[string]string{"logins.json": `[]`})
	if _, err := h.Credential(context.Background(), host.Account{Pin: "codex/nope", Provider: "codex"}); err == nil || errors.Is(err, host.ErrFormat) {
		t.Errorf("unknown account: err = %v", err)
	}
}

// The default token file holds whichever account agy itself is signed in
// to. An entry that is not known to be agy's own must not read it, or the
// quota and fingerprint of that account would go to this pin.
func TestAgyDefaultFileOnlyForOwnAccount(t *testing.T) {
	defer func(was string) { goos = was }(goos)
	goos = "linux"
	for name, entry := range map[string]string{
		"home key without agyHome": `{"type":"api","key":"agy:0123456789abcdef","metadata":{"email":"c@example.com"}}`,
		"no key":                   `{"type":"api","metadata":{"email":"c@example.com"}}`,
		"no type":                  `{"key":"agy"}`,
		"empty object":             `{}`,
	} {
		h, home := agyFixture(t, "{}")
		writeFiles(t, h.ConfigDir, map[string]string{"plugin-auth.json": `{"agy-cli#k3x9a2":` + entry + `}`})
		writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agyToken("ya29.other", "1//other", testNow.Add(time.Hour))})
		got, err := h.Credential(context.Background(), agyAccount)
		if !errors.Is(err, host.ErrFormat) || got.AccessToken != "" {
			t.Errorf("%s: %+v, err = %v, want ErrFormat", name, got, err)
		}
	}
}

// A plugin's account is named by nameID of the provider and the name
// only, never by the id its logins.json entry has.
func TestPluginLoginByNameOnly(t *testing.T) {
	h, _ := agyFixture(t, agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour)))
	useGoogle(t, &googleFake{status: 500})
	_, err := h.Credential(context.Background(), host.Account{Pin: "agy-cli/abcdabcdabcdabcd", Provider: "agy-cli"})
	if err == nil || !strings.Contains(err.Error(), "没有账号") {
		t.Fatalf("pin of the entry's id: err = %v, want no account", err)
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
