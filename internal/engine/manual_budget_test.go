package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// cardHost is a renewHost whose refresh answers the account's card
// (host.KeptRefresher). It counts the readings of the kept quota, and
// lists the accounts after listDelay.
type cardHost struct {
	*renewHost
	card      host.KeptQuota
	keptReads int
	keptCtx   func(ctx context.Context)
	listDelay time.Duration
}

func (h *cardHost) RefreshKept(ctx context.Context, pin string) (host.KeptQuota, bool, error) {
	err := h.RefreshQuota(ctx, pin)
	return h.card, h.card.Pin != "", err
}

func (h *cardHost) KeptQuotas(ctx context.Context) ([]host.KeptQuota, error) {
	h.keptReads++
	if h.keptCtx != nil {
		h.keptCtx(ctx)
	}
	return h.fakeHost.KeptQuotas(ctx)
}

func (h *cardHost) Accounts(ctx context.Context) ([]host.Account, error) {
	time.Sleep(h.listDelay)
	return h.fakeHost.Accounts(ctx)
}

// The card the refresh answers is applied as it is; the kept quota is
// read again only when the answer has no card of the account.
func TestManualRefreshAppliesTheRefreshedCard(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	cards := &cardHost{renewHost: renew}
	e.host = cards
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-time.Hour), 70)}
	cards.card = keptClaude("3", c.t, 33)
	if err := manualRefresh(e, "3"); err != nil {
		t.Fatal(err)
	}
	if w := claudeWindow(e); w.Remaining != 33 || cards.keptReads != 0 {
		t.Fatalf("window %+v after %d kept readings", w, cards.keptReads)
	}

	// A card that is no new reading: the refresh failed, and nothing is
	// read again.
	cards.card = keptClaude("3", c.t, 33)
	renew.refresh = func(context.Context, string) error {
		return &host.Error{Code: refreshStaleCode, Message: "Magpie 返回的仍是旧读数"}
	}
	if err := manualRefresh(e, "3"); err == nil || !strings.Contains(err.Error(), "没有读到新额度") || cards.keptReads != 0 {
		t.Fatalf("stale card: err = %v after %d kept readings", err, cards.keptReads)
	}

	// No card of the account: the kept quota is read again.
	renew.refresh = nil
	cards.card = host.KeptQuota{}
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 22)}
	if err := manualRefresh(e, "3"); err != nil {
		t.Fatal(err)
	}
	if w := claudeWindow(e); w.Remaining != 22 || cards.keptReads != 1 {
		t.Fatalf("window %+v after %d kept readings", w, cards.keptReads)
	}
}

// shortManualBudget shortens the time of a manual refresh.
func shortManualBudget(t *testing.T, budget, reread time.Duration) {
	t.Helper()
	was := []time.Duration{manualRefreshBudget, keptRereadWait}
	manualRefreshBudget, keptRereadWait = budget, reread
	t.Cleanup(func() { manualRefreshBudget, keptRereadWait = was[0], was[1] })
}

// A manual refresh ends within manualRefreshBudget from its start, a slow
// account list included, and a refresh that runs out still leaves
// keptRereadWait to read the kept quota.
func TestManualRefreshKeepsTimeForTheKeptReading(t *testing.T) {
	shortManualBudget(t, 600*time.Millisecond, 200*time.Millisecond)
	e, h, renew, c := newRenewEngine(t)
	cards := &cardHost{renewHost: renew, listDelay: 150 * time.Millisecond}
	e.host = cards
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 50)}
	var refreshEnd, keptEnd time.Time
	renew.refresh = func(ctx context.Context, _ string) error {
		refreshEnd, _ = ctx.Deadline()
		<-ctx.Done()
		return &host.Error{Code: "gateway_unreachable", Message: ctx.Err().Error()}
	}
	cards.keptCtx = func(ctx context.Context) {
		keptEnd, _ = ctx.Deadline()
		if ctx.Err() != nil {
			t.Error("no time was left to read the kept quota")
		}
	}
	started := time.Now()
	err := manualRefresh(e, "3")
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "超过手动刷新的时限") {
		t.Fatalf("err = %v", err)
	}
	if w := claudeWindow(e); w.Remaining != 50 {
		t.Fatalf("the kept reading was not applied: %+v", w)
	}
	// The refresh ends keptRereadWait before the budget, counted from
	// before the account list (150ms); the kept reading gets
	// keptRereadWait.
	if limit := started.Add(manualRefreshBudget - keptRereadWait + 20*time.Millisecond); refreshEnd.IsZero() || refreshEnd.After(limit) {
		t.Fatalf("refresh deadline %s after %s", refreshEnd.Sub(started), limit.Sub(started))
	}
	if keptEnd.Sub(refreshEnd) < keptRereadWait-50*time.Millisecond {
		t.Fatalf("kept reading deadline %s after the refresh's", keptEnd.Sub(refreshEnd))
	}
	if elapsed > manualRefreshBudget+100*time.Millisecond {
		t.Fatalf("the manual refresh took %s", elapsed)
	}
}

// A refresh asked by a request with a deadline ends in time for its reply.
func TestManualRefreshFitsTheCallerDeadline(t *testing.T) {
	e, _, renew, _ := newRenewEngine(t)
	var refreshEnd time.Time
	renew.refresh = func(ctx context.Context, _ string) error {
		refreshEnd, _ = ctx.Deadline()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	callerEnd, _ := ctx.Deadline()
	cmd := newCommand(ctx, "refresh", "3")
	e.runCommand(context.Background(), cmd)
	<-cmd.reply
	if want := callerEnd.Add(-replyMargin - keptRereadWait); refreshEnd.IsZero() || !refreshEnd.Equal(want) {
		t.Fatalf("refresh deadline %v, want %v", refreshEnd, want)
	}
}

// A refreshed card Lamplighter cannot read fails the refresh with its
// format error; Magpie's kept reading is still shown.
func TestManualRefreshWithACardThatDoesNotConvert(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	e.host = &cardHost{renewHost: renew}
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 44)}
	renew.refresh = func(context.Context, string) error {
		return fmt.Errorf("POST /v1/magpie/quotas/refresh: claude/3: %w: window \"5 hours\" has no used", host.ErrFormat)
	}
	err := manualRefresh(e, "3")
	if !errors.Is(err, host.ErrFormat) || !strings.Contains(err.Error(), "Magpie 刷新额度失败") {
		t.Fatalf("err = %v", err)
	}
	if w := claudeWindow(e); w.Remaining != 44 {
		t.Fatalf("the kept reading was not shown: %+v", w)
	}
}

// slowHost reports the credentials of the pins in expired as expired. Their
// refreshes and every reading of the kept quota wait until their context
// ends, and so does the account list with listWait.
type slowHost struct {
	*fakeHost
	expired  map[string]bool
	listWait bool
}

func (h *slowHost) Accounts(ctx context.Context) ([]host.Account, error) {
	if h.listWait {
		<-ctx.Done()
	}
	return h.fakeHost.Accounts(ctx)
}

func (h *slowHost) Credential(ctx context.Context, account host.Account) (host.Credential, error) {
	cred, err := h.fakeHost.Credential(ctx, account)
	cred.Expired = h.expired[account.Pin]
	return cred, err
}

func (h *slowHost) RefreshQuota(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (h *slowHost) KeptQuotas(ctx context.Context) ([]host.KeptQuota, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A manual refresh of every account ends within its time, however many
// accounts wait for Magpie: the accounts left when it runs out are not
// queried, and say so.
func TestManualRefreshOfSlowAccountsEndsInTime(t *testing.T) {
	shortManualBudget(t, 600*time.Millisecond, 200*time.Millisecond)
	e, h, _, _ := newRenewEngine(t)
	e.host = &slowHost{fakeHost: h, expired: map[string]bool{"1": true, "2": true, "3": true}}
	started := time.Now()
	if err := manualRefresh(e, ""); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > manualRefreshBudget+150*time.Millisecond {
		t.Fatalf("the manual refresh took %s", elapsed)
	}
	failed := 0
	for _, a := range e.Status().Accounts {
		if a.AuthIndex == "1" || a.AuthIndex == "2" || a.AuthIndex == "3" {
			if !strings.Contains(a.Error, "超过手动刷新的时限") {
				t.Errorf("account %s: error %q", a.AuthIndex, a.Error)
			}
			failed++
		}
	}
	if failed != 3 {
		t.Fatalf("%d slow accounts in the status", failed)
	}

	// An account the refresh has no time left for fails with the same
	// reason.
	e.host.(*slowHost).listWait = true
	err := manualRefresh(e, "1")
	var failure *refreshFailure
	if !errors.As(err, &failure) || failure != errManualTimeUp {
		t.Fatalf("one account: err = %v", err)
	}
}
