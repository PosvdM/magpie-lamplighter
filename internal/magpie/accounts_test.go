package magpie

import (
	"context"
	"errors"
	"io"
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
		"GET /v1/magpie/accounts":        claudeOn,
		"GET /v1/magpie/quotas":          cardsBefore,
		"POST /v1/magpie/quotas/refresh": cardsAfter,
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
	// The limit belongs to the Host: another Host refreshes at once.
	other := &Host{Addr: h.Addr}
	if err := other.RefreshQuota(ctx, "claude/0f0e0d0c0b0a0908"); err != nil {
		t.Fatalf("refresh on another Host: %v", err)
	}
}

// claudeOn is accountsJSON with the Claude account switched on.
var claudeOn = strings.Replace(accountsJSON, `"plan":"max","on":false`, `"plan":"max","on":true`, 1)

// cardsBefore and cardsAfter are the cards of GET /v1/magpie/quotas?kept=1
// and of a refresh that read the Codex and Claude accounts again.
const (
	cardsBefore = `{"object":"list","data":[
 {"provider":"codex","kind":"subscription","user":"a@example.com · Team","readAt":"2026-11-21T07:00:00Z","windows":[]},
 {"provider":"claude","kind":"subscription","user":"b@example.com","readAt":"2026-11-21T07:00:00Z","windows":[]}]}`
	cardsAfter = `{"object":"list","data":[
 {"provider":"codex","name":"Codex","user":"a@example.com · Team","readAt":"2026-11-21T08:00:00Z","windows":[]},
 {"provider":"claude","name":"Claude Code","user":"B@example.com","readAt":"2026-11-21T08:00:00Z","windows":[]}]}`
)

// RefreshQuota checks the account again right before it asks the gateway,
// and tells why it did not.
func TestRefreshQuotaChecksAccount(t *testing.T) {
	noName := strings.Replace(claudeOn, `"user":"b@example.com"`, `"user":" "`, 1)
	for _, c := range []struct {
		name, accounts, pin, code string
	}{
		{"unknown pin", claudeOn, "codex/unknown", codeAccountNotFound},
		{"API key", claudeOn, "deepseek/k1", codeAccountNotFound},
		{"no name", noName, "claude/0f0e0d0c0b0a0908", codeAccountNotFound},
		{"switched off", accountsJSON, "claude/0f0e0d0c0b0a0908", codeAccountOff},
	} {
		g, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts":        c.accounts,
			"GET /v1/magpie/quotas":          cardsBefore,
			"POST /v1/magpie/quotas/refresh": cardsAfter,
		})
		err := h.RefreshQuota(context.Background(), c.pin)
		var hostErr *host.Error
		if !errors.As(err, &hostErr) || hostErr.Code != c.code || hostErr.Message == "" {
			t.Errorf("%s: err = %v, want code %s", c.name, err, c.code)
		}
		for _, r := range g.requests {
			if r.Method == http.MethodPost {
				t.Errorf("%s: sent %s %s", c.name, r.Method, r.URL)
			}
		}
		// A refused account does not use up its refresh: switched on, it
		// refreshes at once.
		g.bodies["GET /v1/magpie/accounts"] = claudeOn
		if c.code == codeAccountOff {
			if err := h.RefreshQuota(context.Background(), c.pin); err != nil {
				t.Errorf("%s: refresh once switched on: %v", c.name, err)
			}
		}
	}
}

// The gateway answers the cards it has when it reads none again: within
// its 30 seconds for a card, when the reading failed or when Magpie puts
// it off. Such a card is refresh_stale, with Magpie's reason told without
// emails or secrets.
func TestRefreshQuotaStale(t *testing.T) {
	const before = `{"object":"list","data":[
 {"provider":"codex","kind":"subscription","user":"a@example.com · Team","readAt":"2026-11-21T07:00:00Z","windows":[]},
 {"provider":"codex","kind":"subscription","user":"a@example.com · Team","from":"office","readAt":"2026-11-21T06:00:00Z","windows":[]}]}`
	card := func(fields string) string {
		return `{"object":"list","data":[{"provider":"claude","user":"b@example.com","readAt":"2026-11-21T09:00:00Z"},` +
			`{"provider":"codex","user":"a@example.com · Team","from":"office","readAt":"2026-11-21T09:00:00Z"},` +
			`{"provider":"codex","user":"a@example.com · Team",` + fields + `}]}`
	}
	secret := "ya29." + strings.Repeat("A1b2", 12)
	for _, c := range []struct {
		name, before, after, want string
	}{
		{"read in the last 30 seconds", before, card(`"readAt":"2026-11-21T07:00:00Z"`), "仍是 2026-11-21 07:00:00 UTC 的读数"},
		{"older reading", before, card(`"readAt":"2026-11-21T06:59:00Z"`), "仍是"},
		{"reading failed, last one shown", before, card(`"readAt":"2026-11-21T07:00:00Z","asOf":"2026-11-21T07:00:00Z","readError":"HTTP 401 for alice.work@example.com: token ` + secret + ` revoked"`), "读取失败：HTTP 401 for ***rk: token … revoked"},
		{"reading failed, none shown", before, card(`"error":"usage endpoint said 503"`), "读取失败：usage endpoint said 503"},
		{"failure despite a new time", before, card(`"readAt":"2026-11-21T08:00:00Z","readError":"timeout"`), "读取失败：timeout"},
		{"put off", before, card(`"readAt":"2026-11-21T07:00:00Z","readDeferred":true`), "暂不读取"},
		{"no time", before, card(`"windows":[]`), "没有给出读取时间"},
		{"no card", before, `{"object":"list","data":[{"provider":"codex","user":"a@example.com · Team","from":"office","readAt":"2026-11-21T09:00:00Z"}]}`, "没有这个账号的额度"},
	} {
		_, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts":        accountsJSON,
			"GET /v1/magpie/quotas":          c.before,
			"POST /v1/magpie/quotas/refresh": c.after,
		})
		err := h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b")
		var hostErr *host.Error
		if !errors.As(err, &hostErr) || hostErr.Code != codeRefreshStale || !strings.Contains(hostErr.Message, c.want) {
			t.Errorf("%s: err = %v, want refresh_stale with %q", c.name, err, c.want)
			continue
		}
		if strings.Contains(hostErr.Message, "@") || strings.Contains(hostErr.Message, "A1b2") {
			t.Errorf("%s: message tells an email or a secret: %s", c.name, hostErr.Message)
		}
	}
	// A reading newer than the kept one, or the first one, is new.
	for name, kept := range map[string]string{"newer": before, "first": `{"object":"list","data":[]}`} {
		_, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts":        accountsJSON,
			"GET /v1/magpie/quotas":          kept,
			"POST /v1/magpie/quotas/refresh": card(`"readAt":"2026-11-21T07:00:01Z"`),
		})
		if err := h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b"); err != nil {
			t.Errorf("%s reading: %v", name, err)
		}
	}
}

func TestShortReason(t *testing.T) {
	long := strings.Repeat("长", reasonLimit+10)
	if got := shortReason(long); []rune(got)[reasonLimit] != '…' || len([]rune(got)) != reasonLimit+1 {
		t.Errorf("long reason: %q", got)
	}
	if got := shortReason("line one\n  <\"a.b@x.io\">  sk-ant-oat01-" + strings.Repeat("x", 40)); got != `line one <"***.b"> …` {
		t.Errorf("reason = %q", got)
	}
}

// A gateway that cannot be reached or answers 5xx is gateway_unavailable;
// a 4xx answer stays the gateway's own error.
func TestRefreshQuotaGatewayUnavailable(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
	}{
		{http.StatusBadGateway, codeGatewayUnavailable},
		{http.StatusServiceUnavailable, codeGatewayUnavailable},
		{http.StatusBadRequest, "gateway_http_error"},
	} {
		h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				io.WriteString(w, claudeOn)
				return
			}
			http.Error(w, `{"error":{"message":"busy"}}`, c.status)
		})
		err := h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b")
		var hostErr *host.Error
		if !errors.As(err, &hostErr) || hostErr.Code != c.code || hostErr.Status != c.status {
			t.Errorf("HTTP %d: err = %v, want code %s", c.status, err, c.code)
		}
	}
	// The account list answers 5xx.
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	})
	var hostErr *host.Error
	if err := h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b"); !errors.As(err, &hostErr) || hostErr.Code != codeGatewayUnavailable {
		t.Errorf("account list 500: err = %v", err)
	}
	// Nothing listens.
	server := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(server.URL, "http://")
	server.Close()
	h = &Host{Addr: addr}
	if err := h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b"); !errors.As(err, &hostErr) || hostErr.Code != codeGatewayUnavailable {
		t.Errorf("unreachable gateway: err = %v", err)
	}
}

// shortDeadlines shortens the deadlines of the gateway calls.
func shortDeadlines(t *testing.T) {
	t.Helper()
	was := []time.Duration{accountsTimeout, keptTimeout, listTimeout, refreshTimeout}
	accountsTimeout, keptTimeout, listTimeout, refreshTimeout = 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() {
		accountsTimeout, keptTimeout, listTimeout, refreshTimeout = was[0], was[1], was[2], was[3]
	})
}

// stallingGateway answers the routes in bodies, and stalls on the route
// stall names: before the response headers ("headers") or halfway through
// the body ("body"), until the request is cancelled.
func stallingGateway(t *testing.T, bodies map[string]string, route, how string) *Host {
	t.Helper()
	release := make(chan struct{})
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != route {
			io.WriteString(w, bodies[r.Method+" "+r.URL.Path])
			return
		}
		if how == "body" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"object":"list","data":[`)
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	return h
}

// The calls the background loop makes end within their deadlines when
// the gateway takes the connection and stops answering, though the
// caller's context never ends.
func TestGatewayCallsEndOnStall(t *testing.T) {
	shortDeadlines(t)
	bodies := map[string]string{
		"GET /v1/magpie/accounts":        claudeOn,
		"GET /v1/magpie/quotas":          cardsBefore,
		"POST /v1/magpie/quotas/refresh": cardsAfter,
		"GET /v1/models":                 modelsSample,
	}
	calls := []struct {
		name  string
		route string
		call  func(h *Host) error
	}{
		{"Accounts", "GET /v1/magpie/accounts", func(h *Host) error { _, err := h.Accounts(context.Background()); return err }},
		{"KeptQuotas", "GET /v1/magpie/quotas", func(h *Host) error { _, err := h.KeptQuotas(context.Background()); return err }},
		{"KeptQuotas' account list", "GET /v1/magpie/accounts", func(h *Host) error { _, err := h.KeptQuotas(context.Background()); return err }},
		{"RefreshQuota", "POST /v1/magpie/quotas/refresh", func(h *Host) error { return h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b") }},
		{"RefreshQuota's account list", "GET /v1/magpie/accounts", func(h *Host) error { return h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b") }},
		{"RefreshQuota's kept reading", "GET /v1/magpie/quotas", func(h *Host) error { return h.RefreshQuota(context.Background(), "codex/1a2b3c4d5e6f7a8b") }},
		{"Models", "GET /v1/models", func(h *Host) error { _, err := h.Models(context.Background()); return err }},
	}
	for _, c := range calls {
		for _, how := range []string{"headers", "body"} {
			h := stallingGateway(t, bodies, c.route, how)
			done := make(chan error, 1)
			go func() { done <- c.call(h) }()
			select {
			case err := <-done:
				if err == nil {
					t.Errorf("%s, stalled at the %s: no error", c.name, how)
				}
				var hostErr *host.Error
				if strings.HasPrefix(c.name, "RefreshQuota") && (!errors.As(err, &hostErr) || hostErr.Code != codeGatewayUnavailable) {
					t.Errorf("%s, stalled at the %s: err = %v, want gateway_unavailable", c.name, how, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s, stalled at the %s: still waiting", c.name, how)
			}
		}
	}
}

// The deadlines stay within what their callers allow: a quota refresh
// takes the gateway's 30 seconds for a card and ends before the 50
// seconds of a tool call through Magpie.
func TestGatewayDeadlines(t *testing.T) {
	if refreshTimeout <= 30*time.Second || refreshTimeout >= 50*time.Second {
		t.Errorf("refreshTimeout = %s", refreshTimeout)
	}
	for name, d := range map[string]time.Duration{"accounts": accountsTimeout, "kept": keptTimeout, "models": listTimeout} {
		if d <= 0 || d > 30*time.Second {
			t.Errorf("%sTimeout = %s", name, d)
		}
	}
}
