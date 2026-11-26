package magpie

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// accountsWithPlansJSON is accountsOnJSON with a plan on the agy-cli
// account.
var accountsWithPlansJSON = strings.Replace(accountsOnJSON, `"user":"c@example.com","on":true`, `"user":"c@example.com","plan":"Google AI Pro","on":true`, 1)

const agyCardJSON = `{"provider":"agy-cli","kind":"subscription","user":"c@example.com","readAt":"2026-11-21T07:58:00Z","windows":[
 {"name":"Gemini · 5 hours","used":10,"resetsAt":"2026-11-21T10:00:00Z","pool":"Gemini"}]}`

// Each card of the kept quota is decoded on its own: a card whose readAt
// is no time, or whose used is text, is logged once and left out, and the
// other cards are returned, each with the plan of the account list.
func TestKeptQuotasSkipsACardThatDoesNotDecode(t *testing.T) {
	cards := `{"data":[
 {"provider":"claude","kind":"subscription","user":"b@example.com","readAt":"yesterday","windows":[]},
 {"provider":"codex","kind":"subscription","user":"a@example.com · Team","readAt":"2026-11-21T07:58:00Z","windows":[{"name":"5 hours","used":"ten","resetsAt":"2026-11-21T10:00:00Z"}]},
 ` + agyCardJSON + `,
 "no card"]}`
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts": accountsWithPlansJSON,
		"GET /v1/magpie/quotas":   cards,
	})
	var logged strings.Builder
	h.Logger = log.New(&logged, "", 0)
	for range 2 {
		got, err := h.KeptQuotas(context.Background())
		if err != nil || len(got) != 1 || got[0].Pin != "agy-cli/ffeeddccbbaa9988" || got[0].Plan != "Google AI Pro" {
			t.Fatalf("kept %+v, %v", got, err)
		}
	}
	text := logged.String()
	if strings.Count(text, "\n") != 3 || !strings.Contains(text, "claude/0f0e0d0c0b0a0908") || !strings.Contains(text, "codex/1a2b3c4d5e6f7a8b") || !strings.Contains(text, "card 4") {
		t.Fatalf("log:\n%s", text)
	}
}

// A card of another account that does not decode, in the kept quota read
// before the refresh or in the refresh's answer, does not fail the
// refresh; the card it answers carries the account's plan.
func TestRefreshKeptSkipsAnotherAccountsBadCard(t *testing.T) {
	clock := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	bad := `{"provider":"codex","kind":"subscription","user":"a@example.com · Team","readAt":7,"windows":[]}`
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        accountsWithPlansJSON,
		"GET /v1/magpie/quotas":          `{"data":[` + bad + `,` + strings.Replace(agyCardJSON, "07:58:00", "07:00:00", 1) + `]}`,
		"POST /v1/magpie/quotas/refresh": `{"data":[` + bad + `,` + agyCardJSON + `]}`,
	})
	h.Logger = log.New(&strings.Builder{}, "", 0)
	k, found, err := h.RefreshKept(context.Background(), "agy-cli/ffeeddccbbaa9988")
	if err != nil || !found || k.Plan != "Google AI Pro" || !k.ReadAt.Equal(time.Date(2026, 11, 21, 7, 58, 0, 0, time.UTC)) {
		t.Fatalf("refresh: %+v, %v, %v", k, found, err)
	}
}

// The account's own card that does not decode is a format error of the
// refresh.
func TestRefreshKeptOwnCardThatDoesNotDecode(t *testing.T) {
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        accountsWithPlansJSON,
		"GET /v1/magpie/quotas":          `{"data":[]}`,
		"POST /v1/magpie/quotas/refresh": `{"data":[{"provider":"agy-cli","kind":"subscription","user":"c@example.com","readAt":"soon","windows":[]}]}`,
	})
	h.Logger = log.New(&strings.Builder{}, "", 0)
	if _, found, err := h.RefreshKept(context.Background(), "agy-cli/ffeeddccbbaa9988"); found || !errors.Is(err, host.ErrFormat) {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

// Right before the refresh is sent, host.RefreshSending is told the time
// the refresh gap counts from. When it refuses, nothing is sent and the
// gap does not start.
func TestRefreshKeptTellsWhenItSends(t *testing.T) {
	clock := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	g, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        accountsWithPlansJSON,
		"GET /v1/magpie/quotas":          `{"data":[]}`,
		"POST /v1/magpie/quotas/refresh": `{"data":[` + agyCardJSON + `]}`,
	})
	const pin = "agy-cli/ffeeddccbbaa9988"
	refused := errors.New("not monitored")
	posts := func() int {
		n := 0
		for _, r := range g.requests {
			if r.Method == "POST" {
				n++
			}
		}
		return n
	}
	ctx := host.WithRefreshSending(context.Background(), func(time.Time) error { return refused })
	if _, _, err := h.RefreshKept(ctx, pin); !errors.Is(err, refused) || posts() != 0 {
		t.Fatalf("refused: err=%v, posts %d", err, posts())
	}
	clock = clock.Add(time.Minute)
	var sent time.Time
	ctx = host.WithRefreshSending(context.Background(), func(at time.Time) error { sent = at; return nil })
	if _, _, err := h.RefreshKept(ctx, pin); err != nil || posts() != 1 || !sent.Equal(clock) {
		t.Fatalf("err=%v, posts %d, sent %v", err, posts(), sent)
	}
	clock = clock.Add(agyRefreshGap - time.Nanosecond)
	if err := h.RefreshQuota(context.Background(), pin); !errors.Is(err, host.ErrRefreshLimited) {
		t.Fatalf("within the gap of the send: %v", err)
	}
}
