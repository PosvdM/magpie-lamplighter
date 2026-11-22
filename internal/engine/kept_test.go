package engine

import (
	"context"
	"errors"
	"slices"
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

// expiredHost reports the credential of pin, and of the pins in also, as
// expired: an OAuth sign-in that a quota refresh renews, or with setup a
// token that it does not.
type expiredHost struct {
	*fakeHost
	pin   string
	also  []string
	setup bool
}

func (h *expiredHost) Credential(ctx context.Context, account host.Account) (host.Credential, error) {
	cred, err := h.fakeHost.Credential(ctx, account)
	cred.Expired = account.Pin == h.pin || slices.Contains(h.also, account.Pin)
	cred.RenewedByRefresh = cred.Expired && !h.setup
	return cred, err
}

// renewHost reports the credential of pin as expired, and records the
// quota refreshes asked of Magpie. A refresh runs refresh when set.
type renewHost struct {
	*expiredHost
	refreshed []string
	refresh   func(ctx context.Context, pin string) error
}

func (h *renewHost) RefreshQuota(ctx context.Context, pin string) error {
	h.refreshed = append(h.refreshed, pin)
	if h.refresh != nil {
		return h.refresh(ctx, pin)
	}
	return nil
}

func newRenewEngine(t *testing.T) (*Engine, *fakeHost, *renewHost, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	h.files["3"] = `{"access_token":"t3","expired":true}`
	renew := &renewHost{expiredHost: &expiredHost{fakeHost: h, pin: "3"}}
	e.host = renew
	return e, h, renew, c
}

// manualRefresh runs a manual refresh command as POST /api/refresh and the
// refresh_quotas tool do.
func manualRefresh(e *Engine, pin string) error {
	cmd := command{kind: "refresh", arg: pin, reply: make(chan commandReply, 1)}
	e.runCommand(context.Background(), cmd)
	return (<-cmd.reply).err
}

func TestManualRefreshAsksMagpieToRefreshAnExpiredCredential(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-30*time.Minute), 70)}
	renew.refresh = func(ctx context.Context, pin string) error {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > manualRefreshBudget {
			t.Errorf("refresh deadline = %v, %v", deadline, ok)
		}
		// Magpie reads the quota again; the gateway keeps the new reading.
		h.kept = []host.KeptQuota{keptClaude("3", c.t, 33)}
		return nil
	}
	if err := manualRefresh(e, "3"); err != nil {
		t.Fatal(err)
	}
	if len(renew.refreshed) != 1 || renew.refreshed[0] != "3" {
		t.Fatalf("refreshed %v, want [3]", renew.refreshed)
	}
	if w := claudeWindow(e); w.Remaining != 33 {
		t.Fatalf("the refreshed reading was not applied: %+v", w)
	}
	// A refresh of all accounts refreshes the expired one only.
	renew.refreshed = nil
	if err := manualRefresh(e, ""); err != nil {
		t.Fatal(err)
	}
	if len(renew.refreshed) != 1 || renew.refreshed[0] != "3" {
		t.Fatalf("refreshed %v, want [3]", renew.refreshed)
	}
}

func TestScheduledRoundsRenewAnExpiredCredentialAtMostOncePerGap(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	ctx := context.Background()
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-2*time.Hour), 70)}
	renew.refresh = func(ctx context.Context, pin string) error {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > scheduledRenewTimeout {
			t.Errorf("refresh deadline = %v, %v", deadline, ok)
		}
		h.kept = []host.KeptQuota{keptClaude("3", c.t, 33)}
		return nil
	}
	e.poll(ctx, e.config(), c.t, "", false)
	if len(renew.refreshed) != 1 || renew.refreshed[0] != "3" {
		t.Fatalf("refreshed %v, want [3]", renew.refreshed)
	}
	if w := claudeWindow(e); w.Remaining != 33 {
		t.Fatalf("the refreshed reading was not applied: %+v", w)
	}
	// Within the gap, scheduled rounds and the reset probes read the kept
	// quota only.
	c.add(scheduledRenewGap - time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	e.poll(ctx, e.config(), c.t, "3", true)
	if len(renew.refreshed) != 1 {
		t.Fatalf("refreshed %v within the gap", renew.refreshed)
	}
	// A failed refresh counts too: the next one waits for the gap.
	c.add(time.Minute)
	renew.refresh = func(context.Context, string) error { return errors.New("Claude Code 启动失败") }
	e.poll(ctx, e.config(), c.t, "", false)
	c.add(time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	if len(renew.refreshed) != 2 {
		t.Fatalf("refreshed %v, want two refreshes", renew.refreshed)
	}
	if w := claudeWindow(e); w.Remaining != 33 {
		t.Fatalf("the kept reading was not kept: %+v", w)
	}
	// Ignition's query after a start does not refresh.
	c.add(scheduledRenewGap)
	if err := e.queryCred(ctx, e.config(), e.creds["3"], false); err != nil {
		t.Fatal(err)
	}
	if len(renew.refreshed) != 2 {
		t.Fatalf("refreshed %v outside a scheduled round", renew.refreshed)
	}
	// A token the refresh does not renew, such as a Claude setup-token,
	// only reads the kept quota.
	renew.setup = true
	e.poll(ctx, e.config(), c.t, "", false)
	if len(renew.refreshed) != 2 {
		t.Fatalf("refreshed %v for a setup-token", renew.refreshed)
	}
}

func TestScheduledRoundsRenewOneAccountPerRound(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	ctx := context.Background()
	renew.also = []string{"1"}
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 60)}
	renew.refresh = func(context.Context, string) error { return nil }
	c.add(time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	if len(renew.refreshed) != 1 {
		t.Fatalf("refreshed %v in one round, want one account", renew.refreshed)
	}
	c.add(5 * time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	if len(renew.refreshed) != 2 || renew.refreshed[0] == renew.refreshed[1] {
		t.Fatalf("refreshed %v, want each account once over two rounds", renew.refreshed)
	}
}

func TestFailedRefreshFallsBackToTheKeptQuota(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 44)}
	renew.refresh = func(context.Context, string) error { return errors.New("额度刷新过于频繁") }
	err := manualRefresh(e, "3")
	if err == nil || !strings.Contains(err.Error(), "额度刷新过于频繁") {
		t.Fatalf("the refresh error was not reported: %v", err)
	}
	if w := claudeWindow(e); w.Remaining != 44 {
		t.Fatalf("the kept reading was not applied: %+v", w)
	}
	for _, account := range e.Status().Accounts {
		if account.AuthIndex == "3" && !strings.Contains(account.Error, "额度刷新过于频繁") {
			t.Fatalf("account error = %q", account.Error)
		}
	}
	// Without a kept reading both errors are reported.
	h.kept = nil
	err = manualRefresh(e, "3")
	if err == nil || !strings.Contains(err.Error(), "额度刷新过于频繁") || !strings.Contains(err.Error(), "没有这个账号的额度记录") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshesShareTheManualBudget(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 50)}
	renew.refresh = func(ctx context.Context, _ string) error { return ctx.Err() }
	cfg := e.config()
	renewCtx, cancel := context.WithCancelCause(context.Background())
	cancel(errManualTimeUp)
	// A refresh whose budget has run out still applies the kept reading.
	err := e.queryCredWith(context.Background(), cfg, e.creds["3"], true, renewCtx, false)
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("err = %v", err)
	}
	if w := claudeWindow(e); w.Remaining != 50 {
		t.Fatalf("the kept reading was not applied: %+v", w)
	}
}

func TestRefreshQuotasToolRefreshesOneExpiredAccount(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	c.add(time.Minute)
	renew.refresh = func(context.Context, string) error {
		h.kept = []host.KeptQuota{keptClaude("3", c.t, 21)}
		return nil
	}
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	go func() {
		cmd := <-e.cmdCh
		e.runCommand(context.Background(), cmd)
	}()
	value, err := NewMCPBackend(e).refresh(context.Background(), map[string]string{"account": "me@example.com"})
	if err != nil || value.(map[string]any)["ok"] != true {
		t.Fatalf("refresh_quotas = %#v, %v", value, err)
	}
	if len(renew.refreshed) != 1 || renew.refreshed[0] != "3" {
		t.Fatalf("refreshed %v, want [3]", renew.refreshed)
	}
	if w := claudeWindow(e); w.Remaining != 21 {
		t.Fatalf("the refreshed reading was not applied: %+v", w)
	}
}
