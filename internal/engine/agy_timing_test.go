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

// A refresh already in flight when the ignition is sent can bring the
// placeholder agy read before it, stamped by Magpie after the request was
// answered: sent 10:00:04, read 10:00:05, ignition sent 10:00:10 and
// answered 10:00:14, stamped 10:00:16. Its reset, 15:00:05, lies before
// the request's time plus 5 hours, as a window running since before would.
// Only a refresh sent magpieConfirmAfter or more after the answer
// confirms, so the confirming refresh does.
func TestAgyReadingBeganBeforeTheIgnitionIsNoConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	c.add(4 * time.Second)
	e.queueAgyManual("4")
	job, ok := e.nextAgyJob(c.t)
	if !ok {
		t.Fatal("no job")
	}
	world.refresh = func() *host.KeptQuota {
		read := c.t.Add(time.Second)
		r := agyReading("4", read.Add(11*time.Second), agyPool("Gemini", 100, read.Add(5*time.Hour)))
		return &r
	}
	inFlight := agyDone{job: job, result: e.runAgyJob(ctx, job)}
	world.refresh = nil
	c.add(6 * time.Second)
	sent := c.t
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		c.add(4 * time.Second)
		return host.ModelResponse{StatusCode: 200}, nil
	}
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	c.add(2 * time.Second)
	e.agyFinished(ctx, e.config(), inFlight)
	if !e.confirmPending(key) || len(eventsOf(e, "ignite")) != 0 {
		t.Fatalf("the refresh sent before the ignition confirmed it: events %+v", e.Status().Events)
	}
	// Nor does a refresh sent a second after the answer, whatever its
	// card shows.
	early := agyReading("4", c.t.Add(time.Second), agyPool("Gemini", 99.9, sent.Add(5*time.Hour+time.Second)))
	e.confirmByRefresh(ctx, e.config(), agyDone{job: agyJob{pin: "4", kind: agyManual}, result: refreshResult{
		cards: true, answered: true, card: early, sentAt: sent.Add(5 * time.Second)}})
	if !e.confirmPending(key) {
		t.Fatal("a refresh sent a second after the answer confirmed the ignition")
	}
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99.98, sent.Add(5*time.Hour+time.Second)))
		return &r
	}
	c.add(magpieConfirmAfter)
	workAgy(e)
	if e.confirmPending(key) || len(eventsOf(e, "ignite")) != 1 || refreshCount(h) != 2 || !h.confirming[1] {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
}

// Magpie and the agy-cli plugin join a read of the account under way, so
// the confirming refresh, sent magpieConfirmAfter after the answer, can
// bring a placeholder agy read before the answer. Its reset can lie
// within the request's times plus 5 hours, but nothing of it is used: it
// does not confirm the ignition. The next refresh reads the window the
// ignition started, with quota used.
func TestAgyJoinedReadIsNoConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	sent := c.t
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		c.add(4 * time.Second)
		return host.ModelResponse{StatusCode: 200}, nil
	}
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	// agy began reading at 10:00:03; Magpie stamps it at 10:00:20.
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", sent.Add(20*time.Second), agyPool("Gemini", 100, sent.Add(5*time.Hour+3*time.Second)))
		return &r
	}
	c.add(magpieConfirmAfter)
	workAgy(e)
	if !e.confirmPending(key) || len(eventsOf(e, "ignite")) != 0 || refreshCount(h) != 1 {
		t.Fatalf("a placeholder with nothing used confirmed the ignition: events %+v", e.Status().Events)
	}
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99.97, sent.Add(5*time.Hour+time.Second)))
		return &r
	}
	c.add(time.Minute)
	workAgy(e)
	if e.confirmPending(key) || len(eventsOf(e, "ignite")) != 1 {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
}

// A window that runs moves to the next cycle: its first reading, with
// nothing used yet, resets hours later than the last one. That jump is no
// rolling placeholder; two placeholders whose resets moved as far as the
// readings did are.
func TestAgyNewCycleIsNotRolling(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	c.add(time.Minute)
	r := agyReading("4", c.t, agyPool("Gemini", 60, c.t.Add(2*time.Hour)))
	world.reading = &r
	e.pollKept(ctx)
	c.add(2*time.Hour + 30*time.Minute)
	world.reading = slidingReading(c.t, 20*time.Second)
	e.pollKept(ctx)
	if e.state.Target(key).RollingReset {
		t.Fatal("the reset of the next cycle, hours later, was taken for a rolling placeholder")
	}
	c.add(15 * time.Minute)
	world.reading = slidingReading(c.t, 2*time.Second)
	e.pollKept(ctx)
	if !e.state.Target(key).RollingReset {
		t.Fatal("two placeholders 15 minutes apart are not rolling")
	}
}

// The same card applied again, as after a restart that forgot which
// readings were applied, is no second observation: the window stays
// rolling, and its failure protection stays.
func TestAgySameCardAgainIsNoSecondObservation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	c.add(time.Minute)
	world.reading = slidingReading(c.t, time.Second)
	e.pollKept(ctx)
	c.add(15 * time.Minute)
	world.reading = slidingReading(c.t, 3*time.Second)
	e.pollKept(ctx)
	ts := e.state.Target(key)
	if !ts.RollingReset {
		t.Fatal("setup: not rolling")
	}
	ts.ConsecutiveFailures, ts.CircuitOpenUntilEpoch = 1, float64(c.t.Add(10*time.Hour).Unix())
	delete(e.keptAt, "4")
	e.pollKept(ctx)
	if !ts.RollingReset || ts.CircuitOpenUntilEpoch == 0 || ts.ConsecutiveFailures != 1 {
		t.Fatalf("the same card again: %+v", ts)
	}
}

// A Claude reading of Magpie's kept quota is observed as before
// (ignite.Observe), not by the rule for Antigravity: the first reading of a
// new window, which resets about 5 hours after it and hours after the last
// one, is no rolling placeholder; and a placeholder's reset is to move as
// far as the readings did within 2%, at most 10 seconds, not 120.
func TestKeptReadingOfClaudeIsObservedAsBefore(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	key := "claude:3:claude:main"
	apply := func(remaining float64, reset time.Time) bool {
		k := keptClaude("3", c.t, remaining)
		k.Groups[0].Windows[0].Reset = reset
		e.applyKeptQuota(ctx, k)
		return e.state.Target(key).RollingReset
	}
	c.add(time.Minute)
	apply(40, c.t.Add(time.Hour))
	c.add(2 * time.Hour)
	if apply(99, c.t.Add(5*time.Hour-30*time.Second)) {
		t.Fatal("the new window of a Claude account was taken for a rolling placeholder")
	}
	c.add(10 * time.Minute)
	if apply(100, c.t.Add(5*time.Hour-90*time.Second)) {
		t.Fatal("a reset that moved a minute less than the readings did was taken for a rolling placeholder")
	}
	c.add(10 * time.Minute)
	if !apply(100, c.t.Add(5*time.Hour-90*time.Second)) {
		t.Fatal("a reset that moved as far as the readings did is not rolling")
	}
}

// When ignition stops while the last confirming refresh runs, the
// refresh, cut short, is no try: nothing is recorded, the ignition is not
// paused, and the confirmation goes on when the loop runs again.
func TestAgyStopDuringTheLastConfirmationIsNoFailure(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	key := agyTarget(t, e).group.Key
	world.refresh = func() *host.KeptQuota { return nil }
	e.performDue(context.Background(), e.config())
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	c.add(61 * time.Second)
	workAgy(e)
	c.add(61 * time.Second)
	job, ok := e.nextAgyJob(c.t)
	if !ok || job.kind != agyConfirm {
		t.Fatalf("job %+v", job)
	}
	before := e.agyErrs["4"]
	stopped, stop := context.WithCancel(context.Background())
	stop()
	e.agyFinished(stopped, e.config(), agyDone{job: job, result: e.runAgyJob(stopped, job)})
	ts := e.state.Target(key)
	confirm := e.magpieConfirms[key]
	if confirm == nil || confirm.done != 2 || confirm.running || e.agyBusy != nil {
		t.Fatalf("confirmation %+v", confirm)
	}
	if ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 || ts.LastError != "" || len(eventsOf(e, "ignite_failed"))+len(eventsOf(e, "ignite_paused")) != 0 {
		t.Fatalf("target %+v, events %+v", ts, e.Status().Events)
	}
	if e.agyErrs["4"] != before {
		t.Fatalf("refresh error %v", e.agyErrs["4"])
	}
	workAgy(e)
	if refreshCount(h) != 3 || e.confirmPending(key) || len(eventsOf(e, "ignite_paused")) != 1 {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
}

// gapHost is the Magpie of agyHost with package magpie's refresh gap: it
// sends a refresh send later than asked, counts the gap from the send,
// and refuses a refresh within it unsent.
type gapHost struct {
	*agyCards
	c        *clock
	send     time.Duration
	lastSent time.Time
	sends    []time.Time
}

func (h *gapHost) RefreshKept(ctx context.Context, pin string) (host.KeptQuota, bool, error) {
	h.c.add(h.send)
	if !h.lastSent.IsZero() && h.c.t.Sub(h.lastSent) < magpieRefreshGap {
		return host.KeptQuota{}, false, fmt.Errorf("%w：%s", host.ErrRefreshLimited, pin)
	}
	before := refreshCount(h.fakeHost)
	k, found, err := h.agyCards.RefreshKept(ctx, pin)
	if refreshCount(h.fakeHost) > before {
		h.lastSent = h.c.t
		h.sends = append(h.sends, h.c.t)
	}
	return k, found, err
}

// The engine counts an account's refresh gap from when the host sent the
// refresh, as the host does, so its scheduled refresh is not refused; a
// manual refresh the host refused within the gap does not push the next
// scheduled one back.
func TestAgyRefreshGapCountsFromTheSend(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	world.refresh = func() *host.KeptQuota { return nil }
	gaps := &gapHost{agyCards: e.host.(*agyCards), c: c, send: 10 * time.Second}
	e.host = gaps
	for i := 0; i < 50; i++ {
		c.add(time.Minute)
		e.poll(ctx, e.config(), c.t, "", false)
		if i == 20 {
			e.queueAgyManual("4")
		}
		workAgy(e)
	}
	if len(gaps.sends) < 3 {
		t.Fatalf("sent %v", gaps.sends)
	}
	for i := 1; i < len(gaps.sends); i++ {
		if d := gaps.sends[i].Sub(gaps.sends[i-1]); d > magpieRefreshGap+time.Minute+gaps.send {
			t.Fatalf("refresh %d came %v after the one before: %v", i, d, gaps.sends)
		}
	}
}

// Monitoring turned off for Antigravity, before the accounts are loaded
// again, ends the account's refreshes: the queued ones do not run, the
// manual refresh waiting for it is told, the confirmation ends without a
// failure, and a refresh the worker took is not sent.
func TestAgyRefreshesStopWhenMonitoringIsOff(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	monitor := func(on bool) {
		cfg := e.config()
		p := cfg.Provider("antigravity")
		p.Monitor = on
		cfg.Providers["antigravity"] = p
		e.Configure(cfg, nil)
	}
	// A job the worker took before the change is not sent.
	e.queueAgyManual("4")
	job, _ := e.nextAgyJob(c.t)
	monitor(false)
	e.agyFinished(ctx, e.config(), agyDone{job: job, result: e.runAgyJob(ctx, job)})
	if refreshCount(h) != 0 || e.agyBusy != nil {
		t.Fatalf("refreshed %v", h.refreshed)
	}
	monitor(true)
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	waiting := e.queueAgyManual("4")
	e.agyStale["4"] = true
	monitor(false)
	c.add(magpieConfirmDelays[0])
	e.pruneAgy(ctx, e.config())
	if job, ok := e.nextAgyJob(c.t); ok {
		t.Fatalf("job %+v taken", job)
	}
	if refreshCount(h) != 0 || e.confirmPending(key) || len(e.agyStale) != 0 || len(e.agyManualQ) != 0 {
		t.Fatalf("refreshed %v, stale %v, manual %v", h.refreshed, e.agyStale, e.agyManualQ)
	}
	if err := <-waiting; !errors.Is(err, errRefreshAccountGone) {
		t.Fatalf("manual refresh: %v", err)
	}
	ts := e.state.Target(key)
	if ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 || len(eventsOf(e, "ignite_failed")) != 0 {
		t.Fatalf("target %+v", ts)
	}
}

// A reading Magpie gives with a new plan in its account list is compared
// with the baselines of the new plan: the plan is recorded first, and the
// fresh quota of the new plan is no recovery.
func TestAgyPlanIsRecordedBeforeTheReading(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	sender := &switchSender{}
	e.alerts.Sender = sender
	weekly := c.t.Add(3 * 24 * time.Hour)
	c.add(time.Minute)
	r := agyReading("4", c.t, withWeekly(agyPool("Gemini", 100, c.t.Add(-time.Minute)), "Gemini", 3, weekly))
	r.Plan, r.PlanAt = "Google AI Pro", c.t
	world.reading = &r
	e.pollKept(ctx)
	sender.titles()
	c.add(time.Minute)
	next := agyReading("4", c.t, withWeekly(agyPool("Gemini", 100, c.t.Add(-time.Minute)), "Gemini", 100, weekly))
	next.Plan, next.PlanAt = "Google AI Ultra", c.t
	world.reading = &next
	e.pollKept(ctx)
	if got := sender.titles(); agPlan(e) != "Ultra" || len(got) != 1 || !strings.Contains(got[0], "Pro → Ultra") {
		t.Fatalf("plan %q, sent %v", agPlan(e), got)
	}
}

// A new reading ends the error of the account's last refresh in the
// status, also when it comes from Magpie's kept quota.
func TestAgyNewReadingClearsTheRefreshError(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	world.refresh = func() *host.KeptQuota { return nil }
	c.add(time.Minute)
	e.queueAgyManual("4")
	workAgy(e)
	errorOf := func() string {
		for _, a := range e.Status().Accounts {
			if a.AuthIndex == "4" {
				return a.Error
			}
		}
		return "missing"
	}
	if errorOf() == "" {
		t.Fatal("setup: no refresh error")
	}
	c.add(time.Minute)
	r := agyReading("4", c.t, agyPool("Gemini", 80, c.t.Add(time.Hour)))
	world.reading = &r
	e.pollKept(ctx)
	if got := errorOf(); got != "" {
		t.Fatalf("error %q after a new reading", got)
	}
}

// A plan a reading brings from an account list read before the one the
// loop last recorded a plan from is older: it is left out, so the plan
// does not change back and the notification baselines stay. A reading's
// plan from a later list is recorded.
func TestAgyOlderPlanOfAReadingIsLeftOut(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	// The worker's refresh reads the account list while Magpie still
	// says Pro; then the loop's round reads it with Ultra.
	c.add(time.Minute)
	listedByTheWorker := c.t
	c.add(time.Second)
	h.auths[0].Plan = "Google AI Ultra"
	e.poll(ctx, e.config(), c.t, "", false)
	if got := agPlan(e); got != "Ultra" {
		t.Fatalf("plan %q", got)
	}
	changes := len(eventsOf(e, "plan_change"))
	c.add(10 * time.Second)
	r := agyReading("4", c.t, agyPool("Gemini", 80, c.t.Add(time.Hour)))
	r.Plan, r.PlanAt = "Google AI Pro", listedByTheWorker
	world.reading = &r
	e.pollKept(ctx)
	if got := agPlan(e); got != "Ultra" || len(eventsOf(e, "plan_change")) != changes {
		t.Fatalf("plan %q, events %+v", got, eventsOf(e, "plan_change"))
	}
	if w := e.groups[agyTarget(t, e).group.Key].Windows[0]; w.Remaining != 80 {
		t.Fatalf("the reading was not applied: %+v", w)
	}
	c.add(time.Minute)
	next := agyReading("4", c.t, agyPool("Gemini", 70, c.t.Add(time.Hour)))
	next.Plan, next.PlanAt = "Google AI Pro", c.t.Add(-time.Second)
	world.reading = &next
	e.pollKept(ctx)
	if got := agPlan(e); got != "Pro" {
		t.Fatalf("a later list's plan: %q", got)
	}
}

// failingRefresh is the Magpie of agyHost whose refresh fails while
// Magpie finishes a new reading: the refresh's card does not convert, or
// the gateway does not answer, and the kept quota read again holds the
// new reading.
type failingRefresh struct {
	*agyCards
	world *agyWorld
	next  func() host.KeptQuota
}

func (h *failingRefresh) RefreshKept(ctx context.Context, pin string) (host.KeptQuota, bool, error) {
	if err := h.RefreshQuota(ctx, pin); err != nil {
		return host.KeptQuota{}, false, err
	}
	r := h.next()
	h.world.reading = &r
	return host.KeptQuota{}, false, fmt.Errorf("POST /v1/magpie/quotas/refresh: %s: %w: bad card", pin, host.ErrFormat)
}

// A refresh that failed but brought a new reading, read from the kept
// quota, leaves no error in the status. The manual refresh waiting for it
// still hears that the refresh failed.
func TestAgyFailedRefreshWithANewReadingLeavesNoError(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	failing := &failingRefresh{agyCards: e.host.(*agyCards), world: world, next: func() host.KeptQuota {
		return agyReading("4", c.t.Add(5*time.Second), agyPool("Gemini", 80, c.t.Add(time.Hour)))
	}}
	e.host = failing
	c.add(time.Minute)
	waiting := e.queueAgyManual("4")
	workAgy(e)
	if err := <-waiting; !errors.Is(err, host.ErrFormat) {
		t.Fatalf("manual refresh: %v", err)
	}
	for _, a := range e.Status().Accounts {
		if a.AuthIndex == "4" && a.Error != "" {
			t.Fatalf("account error %q", a.Error)
		}
	}
	if e.agyErrs["4"] != nil {
		t.Fatalf("refresh error %v", e.agyErrs["4"])
	}
	if w := e.groups[agyTarget(t, e).group.Key].Windows[0]; w.Remaining != 80 {
		t.Fatalf("the new reading was not applied: %+v", w)
	}
	// With no new reading, the failure stays.
	failing.next = func() host.KeptQuota { return *world.reading }
	c.add(magpieRefreshGap)
	waiting = e.queueAgyManual("4")
	workAgy(e)
	if err := <-waiting; !errors.Is(err, host.ErrFormat) || e.agyErrs["4"] == nil {
		t.Fatalf("manual refresh without a new reading: %v", err)
	}
}

// Only the card the refresh itself answered confirms an ignition: a
// reading of the kept quota read after a refresh that failed, which would
// show the window, is applied but confirms nothing.
func TestAgyKeptReadingAfterAFailedRefreshIsNoConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	sent := c.t
	failing := &failingRefresh{agyCards: e.host.(*agyCards), world: world, next: func() host.KeptQuota {
		return agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99, sent.Add(5*time.Hour)))
	}}
	e.host = failing
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	if len(h.refreshed) != 1 || !e.confirmPending(key) || len(eventsOf(e, "ignite")) != 0 {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
	if w := e.groups[key].Windows[0]; w.Remaining != 99 {
		t.Fatalf("the kept reading was not applied: %+v", w)
	}
}

// rereadRefresh is the Magpie of agyHost whose refresh answers a card the
// host read from the kept quota in place of the refresh's own, which did
// not convert (host.KeptQuota Reread): next is that reading.
type rereadRefresh struct {
	*agyCards
	world *agyWorld
	next  func() host.KeptQuota
}

func (h *rereadRefresh) RefreshKept(ctx context.Context, pin string) (host.KeptQuota, bool, error) {
	if err := h.RefreshQuota(ctx, pin); err != nil {
		return host.KeptQuota{}, false, err
	}
	r := h.next()
	h.world.reading = &r
	r.Reread = true
	return r, true, nil
}

// A card the host read from the kept quota after the refresh's own did
// not convert shows the quota and counts as a refresh that worked, but
// confirms no ignition, even with a window that would.
func TestAgyRereadCardIsNoConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	sent := c.t
	e.host = &rereadRefresh{agyCards: e.host.(*agyCards), world: world, next: func() host.KeptQuota {
		return agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99, sent.Add(5*time.Hour)))
	}}
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	if len(h.refreshed) != 1 || !e.confirmPending(key) || len(eventsOf(e, "ignite")) != 0 {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
	if w := e.groups[key].Windows[0]; w.Remaining != 99 || e.agyErrs["4"] != nil {
		t.Fatalf("the reread card was not applied as a refresh that worked: %+v, %v", w, e.agyErrs["4"])
	}
}

// A refresh that failed, whose new reading the loop applied from the kept
// quota while the refresh ran, leaves no error in the status: the same
// card is skipped then, but it ended the error as a new reading does. The
// manual refresh waiting for it still hears that the refresh failed.
func TestAgyFailedRefreshWhoseReadingTheLoopAppliedLeavesNoError(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	e.host = &failingRefresh{agyCards: e.host.(*agyCards), world: world, next: func() host.KeptQuota {
		return agyReading("4", c.t.Add(5*time.Second), agyPool("Gemini", 80, c.t.Add(time.Hour)))
	}}
	c.add(time.Minute)
	waiting := e.queueAgyManual("4")
	job, ok := e.nextAgyJob(e.now())
	if !ok {
		t.Fatal("no job")
	}
	result := e.runAgyJob(ctx, job)
	// The loop reads the kept quota before the result comes back.
	if _, err := e.applyKept(ctx, e.creds["4"]); err != nil {
		t.Fatal(err)
	}
	e.agyFinished(ctx, e.config(), agyDone{job: job, result: result})
	if err := <-waiting; !errors.Is(err, host.ErrFormat) {
		t.Fatalf("manual refresh: %v", err)
	}
	if e.agyErrs["4"] != nil {
		t.Fatalf("refresh error %v", e.agyErrs["4"])
	}
	for _, a := range e.Status().Accounts {
		if a.AuthIndex == "4" && a.Error != "" {
			t.Fatalf("account error %q", a.Error)
		}
	}
	if w := e.groups[agyTarget(t, e).group.Key].Windows[0]; w.Remaining != 80 {
		t.Fatalf("the new reading was not applied: %+v", w)
	}
}
