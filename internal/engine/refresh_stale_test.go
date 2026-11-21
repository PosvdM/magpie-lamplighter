package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// A manual refresh that Magpie ran without a new reading (refresh_stale)
// fails with its reason in POST /api/refresh and refresh_quotas, while
// Magpie's last reading is still shown.
func TestManualRefreshWithoutANewReading(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-30*time.Second), 47)}
	renew.refresh = func(context.Context, string) error {
		return &host.Error{Method: "RefreshQuota", Code: "refresh_stale", Message: "额度卡片有读取错误"}
	}
	err := manualRefresh(e, "3")
	want := "Magpie 这次没有读到新额度：额度卡片有读取错误"
	var failure *refreshFailure
	if !errors.As(err, &failure) || !failure.stale || failure.reason != want || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("POST /api/refresh error %v", err)
	}
	if w := claudeWindow(e); w.Remaining != 47 {
		t.Fatalf("the kept reading was not applied for display: %+v", w)
	}
	for _, account := range e.Status().Accounts {
		if account.AuthIndex == "3" && !strings.Contains(account.Error, want) {
			t.Fatalf("account error = %q", account.Error)
		}
	}
	c.add(time.Minute)
	result := mcpRefresh(t, e, "3")
	if result["ok"] != false || result["error"] != want {
		t.Fatalf("refresh_quotas %#v", result)
	}

	// Without a kept reading both are reported.
	h.kept = nil
	err = manualRefresh(e, "3")
	if !errors.As(err, &failure) || !failure.stale || !strings.Contains(err.Error(), "没有这个账号的额度记录") {
		t.Fatalf("err = %v", err)
	}
}

// A refresh Magpie reports as done counts only when its reading is newer
// than the one applied before: finding the old card is no success.
func TestManualRefreshFindingTheOldReading(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 52)}
	e.pollKept(context.Background())
	renew.refresh = func(context.Context, string) error { return nil }
	err := manualRefresh(e, "3")
	var failure *refreshFailure
	if !errors.As(err, &failure) || !failure.stale || !strings.HasPrefix(err.Error(), "Magpie 这次没有读到新额度") {
		t.Fatalf("POST /api/refresh error %v", err)
	}
	if result := mcpRefresh(t, e, "3"); result["ok"] != false || result["error"] != "Magpie 这次没有读到新额度" {
		t.Fatalf("refresh_quotas %#v", result)
	}
	if w := claudeWindow(e); w.Remaining != 52 {
		t.Fatalf("the kept reading changed: %+v", w)
	}

	// A new reading counts.
	c.add(time.Minute)
	renew.refresh = func(context.Context, string) error {
		h.kept = []host.KeptQuota{keptClaude("3", c.t, 30)}
		return nil
	}
	if err := manualRefresh(e, "3"); err != nil {
		t.Fatal(err)
	}
	if w := claudeWindow(e); w.Remaining != 30 {
		t.Fatalf("the new reading was not applied: %+v", w)
	}
}

// Scheduled rounds never refresh and keep applying Magpie's last reading
// without an error.
func TestScheduledRoundKeepsTheOldReadingWithoutError(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 52)}
	e.pollKept(context.Background())
	e.poll(context.Background(), e.config(), c.t, "", false)
	if len(renew.refreshed) != 0 {
		t.Fatalf("a scheduled round refreshed %v", renew.refreshed)
	}
	for _, account := range e.Status().Accounts {
		if account.AuthIndex == "3" && account.Error != "" {
			t.Fatalf("account error = %q", account.Error)
		}
	}
}
