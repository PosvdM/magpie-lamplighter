package magpie

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// RefreshKept answers the account's card of the refresh as a KeptQuota,
// as KeptQuotas would read it, so that the caller need not read the kept
// quota again.
func TestRefreshKeptAnswersTheCard(t *testing.T) {
	clock := time.Date(2026, 11, 21, 8, 0, 30, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	refreshed := func(readAt string) string {
		return `{"object":"list","data":[
 {"provider":"codex","name":"Codex","user":"a@example.com · Team","readAt":"2026-11-21T08:00:00Z","windows":[{"name":"5 hours","used":99}]},
 {"provider":"claude","name":"Claude Code","user":"B@example.com","readAt":"` + readAt + `","windows":[
  {"name":"5 hours","used":40,"resetsAt":"2026-11-21T12:00:00Z"},
  {"name":"7 days","used":10,"resetSecs":3600},
  {"name":"Extra usage","used":5,"lane":"extra"}]}]}`
	}
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        claudeOn,
		"GET /v1/magpie/quotas":          cardsBefore,
		"POST /v1/magpie/quotas/refresh": refreshed("2026-11-21T08:00:00Z"),
	})
	k, found, err := h.RefreshKept(context.Background(), "claude/0f0e0d0c0b0a0908")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if k.Pin != "claude/0f0e0d0c0b0a0908" || !k.ReadAt.Equal(time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)) || len(k.Groups) != 1 || k.Groups[0].Key != "claude:main" {
		t.Fatalf("kept = %+v", k)
	}
	want := map[string]quota.Window{
		quota.WindowFiveHour: {Remaining: 60, Reset: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)},
		quota.WindowSevenDay: {Remaining: 90, Reset: clock.Add(time.Hour)},
	}
	if len(k.Groups[0].Windows) != len(want) {
		t.Fatalf("windows = %+v", k.Groups[0].Windows)
	}
	for _, w := range k.Groups[0].Windows {
		if wanted, ok := want[w.ID]; !ok || w.Remaining != wanted.Remaining || !w.Reset.Equal(wanted.Reset) {
			t.Errorf("window %+v, want %+v", w, wanted)
		}
	}

	// A card that is no new reading comes with refresh_stale.
	clock = clock.Add(claudeRefreshGap)
	_, h = newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        claudeOn,
		"GET /v1/magpie/quotas":          cardsBefore,
		"POST /v1/magpie/quotas/refresh": refreshed("2026-11-21T07:00:00Z"),
	})
	k, found, err = h.RefreshKept(context.Background(), "claude/0f0e0d0c0b0a0908")
	var hostErr *host.Error
	if !errors.As(err, &hostErr) || hostErr.Code != codeRefreshStale || !found || !k.ReadAt.Equal(time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("stale: %+v found=%v err=%v", k, found, err)
	}

	// An Antigravity card has no windows Lamplighter reads.
	_, h = newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        accountsJSON,
		"GET /v1/magpie/quotas":          `{"object":"list","data":[]}`,
		"POST /v1/magpie/quotas/refresh": `{"object":"list","data":[{"provider":"agy-cli","user":"c@example.com","readAt":"2026-11-21T08:00:00Z","windows":[{"name":"Gemini · 5 hours","used":1}]}]}`,
	})
	if k, found, err := h.RefreshKept(context.Background(), "agy-cli/ffeeddccbbaa9988"); err != nil || found {
		t.Fatalf("agy-cli: %+v found=%v err=%v", k, found, err)
	}
}

// eofBody calls atEOF once its body has been read to the end.
type eofBody struct {
	io.ReadCloser
	atEOF func()
}

func (b eofBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && b.atEOF != nil {
		b.atEOF()
	}
	return n, err
}

// A caller that ends after the readings before the refresh sends no
// refresh, and does not start the account's gap: a Claude account is
// refreshed at once by the next call.
func TestRefreshSendsNothingAfterTheCallerEnds(t *testing.T) {
	g, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts":        claudeOn,
		"GET /v1/magpie/quotas":          cardsBefore,
		"POST /v1/magpie/quotas/refresh": cardsAfter,
	})
	ctx, cancel := context.WithCancel(context.Background())
	old := gatewayClient
	gatewayClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := gatewayTransport.RoundTrip(r)
		if err == nil && r.Method == http.MethodGet && r.URL.Path == "/v1/magpie/quotas" {
			// The kept reading arrives whole; the caller ends right after.
			resp.Body = eofBody{ReadCloser: resp.Body, atEOF: cancel}
		}
		return resp, err
	})}
	t.Cleanup(func() { gatewayClient = old })

	if err := h.RefreshQuota(ctx, "claude/0f0e0d0c0b0a0908"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	for _, r := range g.requests {
		if r.Method == http.MethodPost {
			t.Fatalf("sent %s %s after the caller ended", r.Method, r.URL)
		}
	}
	if err := h.RefreshQuota(context.Background(), "claude/0f0e0d0c0b0a0908"); err != nil {
		t.Fatalf("the next refresh: %v", err)
	}
}
