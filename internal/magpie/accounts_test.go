package magpie

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// accountsJSON is a GET /v1/magpie/accounts answer in the shape of
// listedAccount in Magpie's internal/gateway/accounts_list.go.
const accountsJSON = `{"object":"list","data":[
 {"provider":"codex","providerName":"Codex","id":"1a2b3c4d5e6f7a8b","pin":"codex/1a2b3c4d5e6f7a8b","kind":"account","label":"a@example.com · Team","user":"a@example.com · Team","plan":"team","on":true},
 {"provider":"claude","providerName":"Claude Code","id":"0f0e0d0c0b0a0908","pin":"claude/0f0e0d0c0b0a0908","kind":"account","label":"主力号","user":"b@example.com","plan":"max","on":false},
 {"provider":"agy-cli","providerName":"Antigravity (local agy CLI)","id":"ffeeddccbbaa9988","pin":"agy-cli/ffeeddccbbaa9988","kind":"account","label":"c@example.com","user":"c@example.com","on":true},
 {"provider":"deepseek","id":"k1","pin":"deepseek/k1","kind":"key","label":"sk-…abcd","on":true}]}`

// gateway is a fake Magpie gateway: each path answers its body, and the
// requests are recorded.
type gateway struct {
	*httptest.Server
	bodies   map[string]string
	requests []*http.Request
}

func newGateway(t *testing.T, bodies map[string]string) (*gateway, *Host) {
	t.Helper()
	g := &gateway{bodies: bodies}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests = append(g.requests, r)
		body, ok := g.bodies[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, `{"error":{"message":"no route"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(g.Close)
	return g, &Host{Addr: strings.TrimPrefix(g.URL, "http://"), ConfigDir: t.TempDir()}
}

func TestAccounts(t *testing.T) {
	g, h := newGateway(t, map[string]string{"GET /v1/magpie/accounts": accountsJSON})
	got, err := h.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []host.Account{
		{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex", Label: "a@example.com · Team", Email: "a@example.com", User: "a@example.com · Team", Plan: "team", On: true},
		{Pin: "claude/0f0e0d0c0b0a0908", Provider: "claude", Label: "主力号", Email: "b@example.com", User: "b@example.com", Plan: "max", On: false},
		{Pin: "agy-cli/ffeeddccbbaa9988", Provider: "agy-cli", Label: "c@example.com", Email: "c@example.com", User: "c@example.com", On: true},
	}
	if len(got) != len(want) {
		t.Fatalf("accounts = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("account %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The gateway answers loopback without a key, and treats a request
	// with a forwarding header or a browser's headers as another machine's.
	r := g.requests[0]
	for _, name := range []string{"Authorization", "X-Forwarded-For", "Forwarded", "Origin", "Sec-Fetch-Site"} {
		if r.Header.Get(name) != "" {
			t.Errorf("request carries %s", name)
		}
	}
	if strings.HasPrefix(r.UserAgent(), "Mozilla/") {
		t.Errorf("User-Agent %q looks like a browser", r.UserAgent())
	}
}

func TestAccountsFormat(t *testing.T) {
	for _, body := range []string{
		`[]`,
		`{"object":"list"}`,
		`{"data":[{"provider":"codex","id":"x","pin":"codex/y","kind":"account","on":true}]}`,
		`{"data":[{"provider":"codex","id":"x","pin":"codex/x","kind":"account"}]}`,
		`{"data":[{"provider":"codex","id":"x","pin":"codex/x","kind":"login","on":true}]}`,
	} {
		_, h := newGateway(t, map[string]string{"GET /v1/magpie/accounts": body})
		if _, err := h.Accounts(context.Background()); !errors.Is(err, host.ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", body, err)
		}
	}
}

func TestGatewayError(t *testing.T) {
	_, h := newGateway(t, map[string]string{})
	_, err := h.Accounts(context.Background())
	var hostErr *host.Error
	// The error keeps the body, which failure.Diagnose reads.
	if !errors.As(err, &hostErr) || hostErr.Status != http.StatusNotFound || hostErr.Code != "gateway_http_error" ||
		hostErr.Method != "GET /v1/magpie/accounts" || hostErr.Message != `{"error":{"message":"no route"}}` {
		t.Fatalf("err = %#v", err)
	}
	h = &Host{}
	if _, err := h.Accounts(context.Background()); err == nil || !strings.Contains(err.Error(), "MAGPIE_ADDR") {
		t.Fatalf("no address: err = %v", err)
	}
}

func TestGatewayURL(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:3425":         "http://127.0.0.1:3425/v1/models",
		":3425":                  "http://127.0.0.1:3425/v1/models",
		"0.0.0.0:3425":           "http://127.0.0.1:3425/v1/models",
		"[::]:3425":              "http://127.0.0.1:3425/v1/models",
		"192.168.1.5:3425":       "http://192.168.1.5:3425/v1/models",
		"http://127.0.0.1:9/":    "http://127.0.0.1:9/v1/models",
		" localhost:3425 ":       "http://localhost:3425/v1/models",
		"https://magpie.local:1": "https://magpie.local:1/v1/models",
	} {
		got, err := (&Host{Addr: addr}).gatewayURL("/v1/models")
		if err != nil || got != want {
			t.Errorf("gatewayURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
}

func TestRefreshQuota(t *testing.T) {
	clock := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	g, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        accountsJSON,
		"POST /v1/magpie/quotas/refresh": `{"object":"list","data":[]}`,
	})
	ctx := context.Background()
	if err := h.RefreshQuota(ctx, "claude/0f0e0d0c0b0a0908"); err != nil {
		t.Fatal(err)
	}
	last := g.requests[len(g.requests)-1]
	if last.Method != http.MethodPost || last.URL.Query().Get("provider") != "claude" || last.URL.Query().Get("user") != "b@example.com" {
		t.Fatalf("refresh request = %s %s", last.Method, last.URL)
	}
	// A Claude refresh runs Claude Code's /usage: one per claudeRefreshGap.
	clock = clock.Add(claudeRefreshGap - time.Second)
	if err := h.RefreshQuota(ctx, "claude/0f0e0d0c0b0a0908"); !errors.Is(err, ErrRefreshLimited) {
		t.Fatalf("second Claude refresh: err = %v, want ErrRefreshLimited", err)
	}
	clock = clock.Add(time.Second)
	if err := h.RefreshQuota(ctx, "claude/0f0e0d0c0b0a0908"); err != nil {
		t.Fatalf("Claude refresh after the gap: %v", err)
	}
	if err := h.RefreshQuota(ctx, "codex/1a2b3c4d5e6f7a8b"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(refreshGap)
	if err := h.RefreshQuota(ctx, "codex/1a2b3c4d5e6f7a8b"); err != nil {
		t.Fatalf("Codex refresh after refreshGap: %v", err)
	}
	if err := h.RefreshQuota(ctx, "codex/unknown"); err == nil {
		t.Fatal("refresh of an unknown pin succeeded")
	}
	// The limit belongs to the Host: another Host refreshes at once.
	other := &Host{Addr: h.Addr}
	if err := other.RefreshQuota(ctx, "claude/0f0e0d0c0b0a0908"); err != nil {
		t.Fatalf("refresh on another Host: %v", err)
	}
}
