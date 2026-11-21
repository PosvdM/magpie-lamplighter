package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

func keptClaude(pin string, at time.Time, remaining float64) host.KeptQuota {
	return host.KeptQuota{Pin: pin, ReadAt: at, Groups: []quota.RawGroup{{
		Key: "claude:main", SourceLabel: "Claude",
		Windows: []quota.Window{{ID: quota.WindowFiveHour, Label: "5h", Remaining: remaining, Reset: at.Add(3 * time.Hour), Period: quota.FiveHours}},
	}}}
}

func claudeWindow(e *Engine) WindowStatus {
	for _, account := range e.Status().Accounts {
		if account.AuthIndex == "3" {
			return account.Groups[0].Windows[0]
		}
	}
	return WindowStatus{}
}

func TestKeptQuotasArePassiveData(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	// Readings of accounts that are not loaded yet are kept for later.
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-time.Minute), 60)}
	e.pollKept(ctx)
	if len(e.keptAt) != 0 {
		t.Fatalf("a reading of an unknown account was marked applied: %v", e.keptAt)
	}
	e.poll(ctx, e.config(), c.t, "", false)
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 55), keptClaude("missing", c.t, 10)}
	e.pollKept(ctx)
	if w := claudeWindow(e); w.Source != "passive" || w.Remaining != 55 {
		t.Fatalf("kept reading not applied: %+v", w)
	}
	// The same reading again, or an older one, changes nothing.
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-time.Second), 40)}
	e.pollKept(ctx)
	if w := claudeWindow(e); w.Remaining != 55 {
		t.Fatalf("an older kept reading was applied: %+v", w)
	}
}

func TestExpiredTokenFallsBackToTheKeptQuota(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	before := h.requestsTo(providers.ClaudeUsageURL)
	h.files["3"] = `{"access_token":"t3","expired":true}`
	expired := &expiredHost{fakeHost: h, pin: "3"}
	e.host = expired

	h.kept = []host.KeptQuota{keptClaude("3", c.t, 42)}
	if err := e.refreshCred(ctx, e.config(), e.creds["3"]); err != nil {
		t.Fatal(err)
	}
	if got := h.requestsTo(providers.ClaudeUsageURL); got != before {
		t.Fatalf("an expired token was sent to the vendor %d times", got-before)
	}
	if w := claudeWindow(e); w.Remaining != 42 {
		t.Fatalf("kept reading not applied: %+v", w)
	}
	h.kept = nil
	if err := e.refreshCred(ctx, e.config(), e.creds["3"]); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("no kept reading must be an error: %v", err)
	}
}

// expiredHost reports the credential of pin as expired.
type expiredHost struct {
	*fakeHost
	pin string
}

func (h *expiredHost) Credential(ctx context.Context, account host.Account) (host.Credential, error) {
	cred, err := h.fakeHost.Credential(ctx, account)
	cred.Expired = account.Pin == h.pin
	return cred, err
}
