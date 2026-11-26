package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// keptJSON is a GET /v1/magpie/quotas?kept=1 answer in the shape of
// provider.Quota (internal/provider/quotas.go), with the window names
// Magpie gives Codex (quotaDurationName), Claude (claudeKinds) and an
// agy-cli pool (PooledWindows).
const keptJSON = `{"object":"list","data":[
 {"provider":"codex","name":"Codex","kind":"subscription","plan":"team","user":"A@example.com · Team","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"5 hours","used":12.5,"remaining":87.5,"resetsAt":"2026-11-21T10:00:00Z"},
   {"name":"7 days","used":40,"remaining":60,"resetsAt":"2026-11-25T00:00:00Z"},
   {"name":"30 days","used":5,"remaining":95,"resetsAt":"2026-12-15T00:00:00Z"},
   {"name":"Allowance","used":1,"remaining":99},
   {"name":"Image generation","used":0,"remaining":100,"text":true,"lane":"images","display":"Unknown"}]},
 {"provider":"claude","name":"Claude Code","kind":"subscription","user":"b@example.com","asOf":"2026-11-21T07:30:00Z","windows":[
   {"name":"5 hours","used":0,"remaining":100},
   {"name":"7 days","used":20,"remaining":80,"resetsAt":"2026-11-26T00:00:00Z"},
   {"name":"7 days · Opus","used":30,"remaining":70,"resetsAt":"2026-11-26T00:00:00Z"},
   {"name":"7 days · Fable","used":50,"remaining":50,"resetsAt":"2026-11-27T00:00:00Z"},
   {"name":"Extra usage","used":100,"remaining":0,"text":true,"display":"In use"}]},
 {"provider":"agy-cli","name":"Antigravity","kind":"subscription","user":"c@example.com","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"Gemini · 5 hours","used":10,"remaining":90,"resetsAt":"2026-11-21T10:00:00Z","pool":"Gemini"}]},
 {"provider":"codex","name":"Codex","kind":"subscription","user":"a@example.com · Team","from":"office","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"5 hours","used":99,"remaining":1,"resetsAt":"2026-11-21T10:00:00Z"}]},
 {"provider":"claude","name":"Claude Code","kind":"subscription","user":"nobody@example.com","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"7 days","used":1,"remaining":99,"resetsAt":"2026-11-26T00:00:00Z"}]},
 {"provider":"deepseek","name":"DeepSeek","kind":"balance","balance":"¥10","windows":[]}]}`

// accountsOnJSON is accountsJSON with every account switched on.
var accountsOnJSON = strings.ReplaceAll(accountsJSON, `"on":false`, `"on":true`)

func TestKeptQuotas(t *testing.T) {
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts": accountsOnJSON,
		"GET /v1/magpie/quotas":   keptJSON,
	})
	got, err := h.KeptQuotas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	want := []host.KeptQuota{
		{Pin: "codex/1a2b3c4d5e6f7a8b", ReadAt: at("2026-11-21T07:58:00Z"), Groups: []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{
			{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: 87.5, Reset: at("2026-11-21T10:00:00Z"), Period: quota.FiveHours},
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 60, Reset: at("2026-11-25T00:00:00Z"), Period: quota.SevenDays},
			{ID: "window-30d", Label: "30d", Remaining: 95, Reset: at("2026-12-15T00:00:00Z"), Period: 30 * 24 * time.Hour},
		}}}},
		{Pin: "claude/0f0e0d0c0b0a0908", ReadAt: at("2026-11-21T07:30:00Z"), Groups: []quota.RawGroup{
			{Key: "claude:main", SourceLabel: "Claude", Windows: []quota.Window{
				{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 80, Reset: at("2026-11-26T00:00:00Z"), Period: quota.SevenDays},
			}},
			{Key: "claude:seven-day-opus", SourceLabel: "Claude", Windows: []quota.Window{
				{ID: "seven-day-opus", Label: "7 Day Opus", Remaining: 70, Reset: at("2026-11-26T00:00:00Z"), Period: quota.SevenDays},
			}},
			{Key: providers.FableGroupKey, SourceLabel: "Fable", Windows: []quota.Window{
				{ID: quota.WindowFable, Label: quota.LabelFable, Remaining: 50, Reset: at("2026-11-27T00:00:00Z"), Period: quota.SevenDays},
			}},
		}},
		{Pin: "agy-cli/ffeeddccbbaa9988", ReadAt: at("2026-11-21T07:58:00Z"), Active: true, Groups: []quota.RawGroup{
			{Key: "antigravity:gemini-models", SourceLabel: "Gemini", Windows: []quota.Window{
				{ID: "gemini-5h", Label: quota.LabelFiveHour, Remaining: 90, Reset: at("2026-11-21T10:00:00Z"), Period: quota.FiveHours},
			}},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept quotas:\n got %+v\nwant %+v", got, want)
	}
}

// The kept groups update the windows of the provider's own Fetch only
// when their keys, IDs and labels are the same.
func TestKeptMatchesFetch(t *testing.T) {
	reset := "2026-11-26T00:00:00Z"
	fetched, err := providers.ParseClaudeUsage(map[string]any{
		"five_hour":      map[string]any{"utilization": 10.0, "resets_at": reset},
		"seven_day":      map[string]any{"utilization": 20.0, "resets_at": reset},
		"seven_day_opus": map[string]any{"utilization": 30.0, "resets_at": reset},
		"limits": []any{map[string]any{"kind": "weekly_scoped", "percent": 50.0, "resets_at": reset,
			"scope": map[string]any{"model": map[string]any{"display_name": "Fable"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	used := func(v float64) *float64 { return &v }
	r, _ := time.Parse(time.RFC3339, reset)
	kept, err := keptClaude([]keptWindow{
		{Name: "5 hours", Used: used(10), ResetsAt: &r},
		{Name: "7 days", Used: used(20), ResetsAt: &r},
		{Name: "7 days · Opus", Used: used(30), ResetsAt: &r},
		{Name: "7 days · Fable", Used: used(50), ResetsAt: &r},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shape(kept), shape(fetched)) {
		t.Fatalf("Claude kept %v, fetched %v", shape(kept), shape(fetched))
	}
	fetched, err = providers.ParseCodexUsage(map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": 10.0, "limit_window_seconds": 18000.0, "reset_at": float64(r.Unix())},
		"secondary_window": map[string]any{"used_percent": 20.0, "limit_window_seconds": 2592000.0, "reset_at": float64(r.Unix())},
	}}, r)
	if err != nil {
		t.Fatal(err)
	}
	kept, err = keptCodex([]keptWindow{{Name: "5 hours", Used: used(10), ResetsAt: &r}, {Name: "30 days", Used: used(20), ResetsAt: &r}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shape(kept), shape(fetched)) {
		t.Fatalf("Codex kept %v, fetched %v", shape(kept), shape(fetched))
	}
}

// shape lists each group's key and source label, and each window's ID,
// label, period and remaining quota.
func shape(groups []quota.RawGroup) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.Key+"|"+g.SourceLabel)
		for _, w := range g.Windows {
			out = append(out, w.ID+"|"+w.Label+"|"+w.Period.String()+"|"+strconv.FormatFloat(w.Remaining, 'f', -1, 64)+"|"+w.Reset.UTC().Format(time.RFC3339))
		}
	}
	return out
}

func TestKeptQuotasFormat(t *testing.T) {
	for _, body := range []string{
		`{"object":"list"}`,
		`{"data":{}}`,
	} {
		_, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts": accountsJSON,
			"GET /v1/magpie/quotas":   body,
		})
		if _, err := h.KeptQuotas(context.Background()); !errors.Is(err, host.ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", body, err)
		}
	}
}

// A card that does not convert is logged once and left out; the other
// cards still come, the Codex and Claude readings that stand in for
// expired tokens included. The card of an account switched off is not
// converted at all.
func TestKeptQuotasSkipsABadCard(t *testing.T) {
	const cards = `{"data":[
 {"provider":"agy-cli","kind":"subscription","user":"c@example.com","readAt":"2026-11-21T07:58:00Z","windows":[{"name":"Gemini · 5 hours","pool":"Gemini"}]},
 {"provider":"claude","kind":"subscription","user":"b@example.com","readAt":"2026-11-21T07:58:00Z","windows":[{"name":"5 hours","resetsAt":"2026-11-21T10:00:00Z"}]},
 {"provider":"codex","kind":"subscription","user":"a@example.com · Team","readAt":"2026-11-21T07:58:00Z","windows":[{"name":"5 hours","used":10,"resetsAt":"2026-11-21T10:00:00Z"}]}]}`
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts": accountsJSON,
		"GET /v1/magpie/quotas":   cards,
	})
	var logged strings.Builder
	h.Logger = log.New(&logged, "", 0)
	for range 2 {
		got, err := h.KeptQuotas(context.Background())
		if err != nil || len(got) != 1 || got[0].Pin != "codex/1a2b3c4d5e6f7a8b" {
			t.Fatalf("kept %+v, %v", got, err)
		}
	}
	// The Claude account is off: only the agy-cli card is logged, once.
	if lines := strings.Count(logged.String(), "\n"); lines != 1 || !strings.Contains(logged.String(), "agy-cli/ffeeddccbbaa9988") {
		t.Fatalf("log:\n%s", logged.String())
	}
}

// agyKeptJSON is an agy-cli card of GET ?kept=1: the pools' own windows
// behind the pool's name, a model window that no pool window stands in
// for (PooledWindows keeps it), and a pool without a figure.
const agyKeptJSON = `{"provider":"agy-cli","kind":"subscription","user":"c@example.com","plan":"Google AI Pro","readAt":"2026-11-21T07:58:00Z","windows":[
 {"name":"Gemini · 7 days","used":20.5,"remaining":79.5,"resetsAt":"2026-11-25T00:00:00Z","pool":"Gemini"},
 {"name":"Gemini · 5 hours","used":0.000123,"remaining":99.999877,"resetsAt":"2026-11-21T10:00:00Z","pool":"Gemini"},
 {"name":"Claude & GPT · 5 hours","used":40,"remaining":60,"resetsAt":"2026-11-21T11:00:00Z","pool":"Claude & GPT"},
 {"name":"Claude & GPT · 7 days","used":30,"remaining":70,"resetsAt":"2026-11-26T00:00:00Z","pool":"Claude & GPT"},
 {"name":"Gemini 3.1 Pro","used":20.5,"remaining":79.5,"resetsAt":"2026-11-25T00:00:00Z","pool":"Gemini","family":"Gemini"},
 {"name":"Image · 5 hours","used":0,"remaining":100,"pool":"Image","text":true,"display":"Limits not available"}]}`

// agyRefreshJSON is the same reading as POST /v1/magpie/quotas/refresh
// answers it (provider.SubscriptionQuota): the plugin's names, every
// model window, and resetSecs in place of resetsAt.
const agyRefreshJSON = `{"provider":"agy-cli","name":"Antigravity","user":"c@example.com","readAt":"2026-11-21T07:58:00Z","windows":[
 {"name":"Claude Sonnet","used":40,"resetSecs":10800,"pool":"Claude & GPT","family":"Claude"},
 {"name":"Gemini 3.1 Pro","used":20.5,"resetsAt":"2026-11-25T00:00:00Z","pool":"Gemini","family":"Gemini"},
 {"name":"5 hours","used":0.000123,"resetsAt":"2026-11-21T10:00:00Z","pool":"Gemini"},
 {"name":"7 days","used":20.5,"resetsAt":"2026-11-25T00:00:00Z","pool":"Gemini"},
 {"name":"5 hours","used":40,"resetSecs":10800,"pool":"Claude & GPT"},
 {"name":"7 days","used":30,"resetsAt":"2026-11-26T00:00:00Z","pool":"Claude & GPT"},
 {"name":"5 hours","used":0,"pool":"Image","text":true,"display":"Limits not available"}],
 "routing":[{"model":"claude-sonnet"},{"model":"gemini-3.1-pro"},{},{},{},{},{}]}`

// Both shapes of an agy-cli card give the groups and window IDs that the
// history and the settings (hidden_groups, ignition groups) are keyed by;
// the per-model windows are left out, and a pool without a figure is
// unread.
func TestKeptAgyPools(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	answered := at("2026-11-21T08:00:00Z")
	want := []quota.RawGroup{
		{Key: "antigravity:gemini-models", SourceLabel: "Gemini", Windows: []quota.Window{
			{ID: "gemini-5h", Label: quota.LabelFiveHour, Remaining: 99.999877, Reset: at("2026-11-21T10:00:00Z"), Period: quota.FiveHours},
			{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: 79.5, Reset: at("2026-11-25T00:00:00Z"), Period: quota.SevenDays},
		}},
		{Key: "antigravity:claude-and-gpt-models", SourceLabel: "Claude / GPT", Windows: []quota.Window{
			{ID: "3p-5h", Label: quota.LabelFiveHour, Remaining: 60, Reset: at("2026-11-21T11:00:00Z"), Period: quota.FiveHours},
			{ID: "3p-weekly", Label: quota.LabelSevenDay, Remaining: 70, Reset: at("2026-11-26T00:00:00Z"), Period: quota.SevenDays},
		}},
	}
	var kept keptQuota
	if err := json.Unmarshal([]byte(agyKeptJSON), &kept); err != nil {
		t.Fatal(err)
	}
	var refreshed refreshedCard
	if err := json.Unmarshal([]byte(agyRefreshJSON), &refreshed); err != nil {
		t.Fatal(err)
	}
	fromKept, ok1, err1 := keptOf(kept, "agy-cli/x", answered)
	fromRefresh, ok2, err2 := refreshed.kept("agy-cli/x", answered)
	for name, got := range map[string]struct {
		k   host.KeptQuota
		ok  bool
		err error
	}{"kept": {fromKept, ok1, err1}, "refresh": {fromRefresh, ok2, err2}} {
		if got.err != nil || !got.ok || !reflect.DeepEqual(shape(got.k.Groups), shape(want)) {
			t.Errorf("%s: %v, %v\n got %v\nwant %v", name, got.ok, got.err, shape(got.k.Groups), shape(want))
		}
		// The Image pool has no figure: its group may still exist.
		if !got.k.Active || !reflect.DeepEqual(got.k.Unread, []string{"antigravity:image-models"}) || !got.k.ReadAt.Equal(at("2026-11-21T07:58:00Z")) {
			t.Errorf("%s: active %v, unread %v, read at %v", name, got.k.Active, got.k.Unread, got.k.ReadAt)
		}
	}
}

// Only Magpie's own new reading of an agy-cli account converts. A card
// agy did not read (error), and an earlier reading Magpie shows in place
// of one that failed or was put off (asOf with readError or readDeferred),
// give none, neither 0% nor 100%: Magpie may show a window of such a card
// whose reset has passed as unused and without a reset. A pool without a
// figure is unread, and a card with only such pools gives none.
func TestKeptAgyStates(t *testing.T) {
	answered := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	used := func(v float64) *float64 { return &v }
	read := answered.Add(-time.Minute)
	gemini := []keptWindow{{Name: "Gemini · 5 hours", Pool: "Gemini", Used: used(10)}, {Name: "Gemini · 7 days", Pool: "Gemini", Used: used(5)}}
	// What Magpie's lastQuotas makes of a failed read: the last good
	// reading, its passed 5-hour reset dropped and shown unused.
	fallback := []keptWindow{{Name: "Gemini · 5 hours", Pool: "Gemini", Used: used(0)}, {Name: "Gemini · 7 days", Pool: "Gemini", Used: used(5)}}
	text := keptWindow{Name: "Claude & GPT · 5 hours", Pool: "Claude & GPT", Text: true}
	card := func(q keptQuota) keptQuota {
		q.Provider, q.Kind, q.User = "agy-cli", "subscription", "c@example.com"
		return q
	}
	for name, c := range map[string]struct {
		q      keptQuota
		ok     bool
		unread []string
	}{
		"due for renewal":         {q: card(keptQuota{Error: "agy's sign-in is due to be renewed, and agy renews it itself on this account's next chat", Windows: []keptWindow{}})},
		"still reading":           {q: card(keptQuota{Error: "agy is still reading this account's quota", Windows: []keptWindow{}, ReadAt: &read})},
		"kept after a failure":    {q: card(keptQuota{AsOf: &read, ReadError: "agy is still reading this account's quota", Windows: fallback})},
		"read error with readAt":  {q: card(keptQuota{ReadAt: &read, ReadError: "agy is still reading this account's quota", Windows: fallback})},
		"kept after a deferral":   {q: card(keptQuota{AsOf: &read, ReadDeferred: true, Windows: fallback})},
		"deferred":                {q: card(keptQuota{ReadAt: &read, ReadDeferred: true, Windows: gemini})},
		"only asOf":               {q: card(keptQuota{AsOf: &read, Windows: gemini})},
		"no pool windows":         {q: card(keptQuota{ReadAt: &read, Windows: []keptWindow{{Name: "Gemini 3.1 Pro", Pool: "Gemini", Family: "Gemini", Used: used(1)}}})},
		"only a pool without one": {q: card(keptQuota{ReadAt: &read, Windows: []keptWindow{text}})},
		"every pool read":         {q: card(keptQuota{ReadAt: &read, Windows: gemini}), ok: true},
		"a pool without a figure": {q: card(keptQuota{ReadAt: &read, Windows: append(slices.Clone(gemini), text)}), ok: true, unread: []string{"antigravity:claude-and-gpt-models"}},
	} {
		k, ok, err := keptOf(c.q, "agy-cli/x", answered)
		if err != nil || ok != c.ok || ok && (!k.Active || !reflect.DeepEqual(k.Unread, c.unread)) {
			t.Errorf("%s: ok %v, active %v, unread %v, err %v", name, ok, k.Active, k.Unread, err)
		}
	}
	// A pool window without used is a format Lamplighter does not know.
	if _, _, err := keptOf(card(keptQuota{ReadAt: &read, Windows: []keptWindow{{Name: "5 hours", Pool: "Gemini"}}}), "agy-cli/x", answered); !errors.Is(err, host.ErrFormat) {
		t.Errorf("pool window without used: err = %v", err)
	}
	// A pool without a figure in one window is not read at all.
	k, ok, err := keptOf(card(keptQuota{ReadAt: &read, Windows: []keptWindow{
		{Name: "Gemini · 5 hours", Pool: "Gemini", Text: true}, {Name: "Gemini · 7 days", Pool: "Gemini", Used: used(5)},
		{Name: "Claude & GPT · 7 days", Pool: "Claude & GPT", Used: used(1)},
	}}), "agy-cli/x", answered)
	if err != nil || !ok || len(k.Groups) != 1 || k.Groups[0].Key != "antigravity:claude-and-gpt-models" || !reflect.DeepEqual(k.Unread, []string{"antigravity:gemini-models"}) {
		t.Errorf("half-read pool: %+v, %v, %v", k, ok, err)
	}
}

// agy-cli 0.1.147's cards as Magpie answered them (testdata): an account
// whose windows have not started, and one that agy reads no quota of.
// Magpie lists both, the second switched off; KeptQuotas converts the card
// with a reading, each pool's two windows under the IDs of the history.
func TestKeptAgyCards(t *testing.T) {
	b, err := os.ReadFile("testdata/agy-cli-cards.json")
	if err != nil {
		t.Fatal(err)
	}
	var cards struct{ Kept, Live, Accounts json.RawMessage }
	if err := json.Unmarshal(b, &cards); err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339Nano, s); return v }
	want := []string{
		"antigravity:gemini-models|Gemini",
		"gemini-5h|5 小时|5h0m0s|100|2026-11-26T22:33:47Z",
		"gemini-weekly|7 Day|168h0m0s|100|2026-12-03T17:33:47Z",
		"antigravity:claude-and-gpt-models|Claude / GPT",
		"3p-5h|5 小时|5h0m0s|100|2026-11-26T22:33:47Z",
		"3p-weekly|7 Day|168h0m0s|100|2026-12-03T17:33:47Z",
	}
	for _, body := range []json.RawMessage{cards.Kept, cards.Live} {
		_, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts": string(cards.Accounts),
			"GET /v1/magpie/quotas":   string(body),
		})
		got, err := h.KeptQuotas(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Pin != "agy-cli/a1a1a1a1a1a1a1a1" || !got[0].Active || len(got[0].Unread) != 0 || !got[0].ReadAt.Equal(at("2026-11-26T17:33:47.68376168Z")) {
			t.Fatalf("kept %+v", got)
		}
		if !reflect.DeepEqual(shape(got[0].Groups), want) {
			t.Fatalf("groups\n got %v\nwant %v", shape(got[0].Groups), want)
		}
	}
	// The account list gives the plan.
	_, h := newGateway(t, map[string]string{"GET /v1/magpie/accounts": string(cards.Accounts)})
	accounts, err := h.Accounts(context.Background())
	if err != nil || len(accounts) != 2 || accounts[0].Plan != "Google AI Pro" || accounts[1].On {
		t.Fatalf("accounts %+v, %v", accounts, err)
	}
}
