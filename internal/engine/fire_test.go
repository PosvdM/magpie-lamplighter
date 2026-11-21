package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// gatewayList is a model list in Magpie's naming, "<provider>/<model>".
var gatewayList = []string{
	"codex/gpt-6-luna", "claude/claude-sonnet-4-6", "claude/claude-haiku-4-5-20251001",
	"agy-cli/claude-haiku-4-5-20251001", "agy-cli/gemini-3.8-flash-high", "group/fast",
}

func TestModelsForAccountProvider(t *testing.T) {
	own := credModels(gatewayList, &Cred{Provider: "claude", HostProvider: "claude"})
	if !slices.Equal(own.bare, []string{"claude-sonnet-4-6", "claude-haiku-4-5-20251001"}) || own.id("claude-sonnet-4-6") != "claude/claude-sonnet-4-6" {
		t.Fatalf("%+v", own)
	}
	// The same model of another Magpie provider is left out: a pinned
	// request can only use its own provider's models.
	agy := credModels(gatewayList, &Cred{Provider: "antigravity", HostProvider: "agy-cli"})
	if agy.id("claude-haiku-4-5-20251001") != "agy-cli/claude-haiku-4-5-20251001" || len(agy.bare) != 2 {
		t.Fatalf("%+v", agy)
	}
	// Ids without a provider stay as they are.
	if bare := credModels([]string{"gpt-6-luna"}, &Cred{Provider: "codex", HostProvider: "codex"}); bare.id("gpt-6-luna") != "gpt-6-luna" {
		t.Fatalf("%+v", bare)
	}
	cred := &Cred{Provider: "claude", HostProvider: "claude"}
	if overrideName("claude/claude-sonnet-4-6", cred) != "claude-sonnet-4-6" || overrideName("claude-sonnet-4-6", cred) != "claude-sonnet-4-6" ||
		overrideName("agy-cli/x", cred) != "agy-cli/x" {
		t.Fatal("override names")
	}
	if overrideID("claude-sonnet-4-6", cred) != "claude/claude-sonnet-4-6" || overrideID("claude/x", cred) != "claude/x" {
		t.Fatal("override ids")
	}
}

// With Magpie's prefixed ids, candidates are picked by model name and the
// request names the gateway id of the account's provider.
func TestIgnitionSendsGatewayModelID(t *testing.T) {
	for _, override := range []string{"", "claude-sonnet-4-6", "claude/claude-sonnet-4-6"} {
		c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
		h := claudeIgnitionHost(c, nil)
		h.listModels = func() ([]string, error) { return gatewayList, nil }
		e, _ := newTestEngine(t, h, c)
		ctx := context.Background()
		cfg := e.config()
		cfg.Providers["codex"] = config.Provider{Monitor: true}
		claude := cfg.Providers["claude"]
		claude.Model = override
		cfg.Providers["claude"] = claude
		e.Configure(cfg, nil)
		e.poll(ctx, cfg, c.t, "", false)
		e.performDue(ctx, cfg)
		want := "claude/claude-haiku-4-5-20251001"
		if override != "" {
			want = "claude/claude-sonnet-4-6"
		}
		if len(h.executed) != 1 || h.executed[0].Model != want {
			t.Fatalf("override %q: executed %+v", override, h.executed)
		}
		if next := e.Status().NextModels["claude"]; next != "claude/claude-haiku-4-5-20251001" {
			t.Fatalf("next model %q", next)
		}
	}
}

// warmupHost is a host that reads Magpie's warm-up settings.
type warmupHost struct {
	*fakeHost
	seen []host.Account
	pins []string
	err  error
}

func (h *warmupHost) MagpieWarmup(accounts []host.Account) ([]string, error) {
	h.seen = accounts
	return h.pins, h.err
}

func TestStatusMagpieWarmup(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	// A host without the reader gives an empty list, never null.
	plain, _ := newTestEngine(t, standardHost(c), c)
	if raw, _ := json.Marshal(plain.Status()); !strings.Contains(string(raw), `"magpie_warmup":[]`) {
		t.Fatalf("status without a reader: %s", raw)
	}
	base := standardHost(c)
	h := &warmupHost{fakeHost: base, pins: []string{"3", "1"}}
	e, _ := newTestEngine(t, base, c)
	e.host = h
	e.poll(context.Background(), e.config(), c.t, "", false)
	if got := e.Status().MagpieWarmup; !slices.Equal(got, []string{"1", "3"}) {
		t.Fatalf("warm-up %v", got)
	}
	var claude *host.Account
	for i := range h.seen {
		if h.seen[i].Pin == "3" {
			claude = &h.seen[i]
		}
	}
	if claude == nil || claude.Provider != "claude" || claude.Email != "me@example.com" {
		t.Fatalf("accounts handed over %+v", h.seen)
	}
	h.pins, h.err = nil, errors.New("unknown Magpie format")
	if raw, _ := json.Marshal(e.Status()); !strings.Contains(string(raw), `"magpie_warmup":[]`) {
		t.Fatalf("status after a failed read: %s", raw)
	}
}
