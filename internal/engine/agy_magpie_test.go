package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// agyWorld is the agy-cli account "4" as Magpie reads it: reading is
// Magpie's last reading, nil for none, and refresh, when set, gives the
// reading of each refresh (nil keeps the last one, as a refresh that agy
// did not finish).
type agyWorld struct {
	reading *host.KeptQuota
	refresh func() *host.KeptQuota
}

// agyHost serves only the agy-cli account "4", ignited for its Gemini
// group, with an ended 5-hour window read at c.t.
func agyHost(t *testing.T, c *clock) (*fakeHost, *Engine, *agyWorld) {
	t.Helper()
	h := standardHost(c)
	h.auths = []authEntry{h.auths[4]}
	h.auths[0].Plan = "Google AI Pro"
	world := &agyWorld{}
	r := agyReading("4", c.t, agyPool("Gemini", 100, c.t.Add(-time.Minute)))
	world.reading = &r
	h.agy = func() []host.KeptQuota {
		if world.reading == nil {
			return nil
		}
		return []host.KeptQuota{*world.reading}
	}
	h.onRefresh = func(string) {
		if world.refresh == nil {
			return
		}
		if next := world.refresh(); next != nil {
			world.reading = next
		}
	}
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 200}, nil
	}
	e, _ := newTestEngine(t, h, c)
	e.host = &agyCards{fakeHost: h}
	cfg := e.config()
	p := cfg.Provider("antigravity")
	p.Monitor, p.Ignite = true, true
	cfg.Providers["antigravity"] = p
	e.Configure(cfg, nil)
	e.poll(context.Background(), e.config(), c.t, "", false)
	return h, e, world
}

func agyTarget(t *testing.T, e *Engine) target {
	t.Helper()
	for _, candidate := range e.collectTargets(e.config()) {
		if candidate.group.Provider == "antigravity" {
			return candidate
		}
	}
	t.Fatal("missing Antigravity target")
	return target{}
}

// agyCards is the Magpie of agyHost as package magpie's Host shows it, a
// host.KeptRefresher: a refresh answers the account's card of Magpie's
// reading after it, and is stale when that reading is no newer than the
// one before. A refresh whose ctx has ended is not sent.
type agyCards struct {
	*fakeHost
}

func (h *agyCards) RefreshKept(ctx context.Context, pin string) (host.KeptQuota, bool, error) {
	if err := ctx.Err(); err != nil {
		return host.KeptQuota{}, false, err
	}
	card := func() (host.KeptQuota, bool) {
		for _, k := range h.agy() {
			if k.Pin == pin {
				return k, true
			}
		}
		return host.KeptQuota{}, false
	}
	before, _ := card()
	if err := h.RefreshQuota(ctx, pin); err != nil {
		return host.KeptQuota{}, false, err
	}
	after, found := card()
	if !found || !after.ReadAt.After(before.ReadAt) {
		return after, found, &host.Error{Code: refreshStaleCode, Message: "no new reading"}
	}
	return after, true, nil
}

// workAgy runs, in the loop's place, the jobs of the background worker due
// at the clock's time, one after the other.
func workAgy(e *Engine) {
	ctx := context.Background()
	e.pruneAgy(ctx, e.config())
	for {
		job, ok := e.nextAgyJob(e.now())
		if !ok {
			return
		}
		e.agyFinished(ctx, e.config(), agyDone{job: job, result: e.runAgyJob(ctx, job)})
	}
}

func refreshCount(h *fakeHost) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.refreshed)
}

// Lamplighter asks Google nothing for an agy-cli account: its quota and
// plan come from Magpie, and each pool of a reading is applied as an
// active query.
func TestAgyQuotaComesFromMagpie(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	if len(h.requests) != 0 {
		t.Fatalf("requests %+v", h.requests)
	}
	if got := agPlan(e); got != "Pro" {
		t.Fatalf("plan %q", got)
	}
	groups := func() string {
		var out []string
		for _, a := range e.Status().Accounts {
			for _, g := range a.Groups {
				out = append(out, g.SourceLabel+"="+string(rune('0'+len(g.Windows))))
			}
		}
		return strings.Join(out, ",")
	}
	// A reading adds the Claude / GPT pool.
	c.add(time.Minute)
	r := agyReading("4", c.t, agyPool("Gemini", 80, c.t.Add(time.Hour)), agyPool("Claude & GPT", 100, c.t.Add(time.Hour)))
	world.reading = &r
	e.pollKept(context.Background())
	if got := groups(); got != "Gemini=1,Claude / GPT=1" {
		t.Fatalf("groups %s", got)
	}
	// A reading no newer than the last one applied is skipped whole.
	old := agyReading("4", c.t, agyPool("Gemini", 10, c.t.Add(time.Hour)))
	world.reading = &old
	e.pollKept(context.Background())
	if w := e.groups["antigravity:4:antigravity:gemini-models"].Windows[0]; w.Remaining != 80 {
		t.Fatalf("an old reading was applied: %+v", w)
	}
	// A pool agy gave no figure for stays as it is.
	c.add(time.Minute)
	partial := agyReading("4", c.t, agyPool("Gemini", 70, c.t.Add(time.Hour)))
	partial.Unread = []string{"antigravity:claude-and-gpt-models"}
	world.reading = &partial
	e.pollKept(context.Background())
	if got := groups(); got != "Gemini=1,Claude / GPT=1" {
		t.Fatalf("after a reading without a figure for a pool: %s", got)
	}
	// A reading without the pool keeps its group as a tombstone.
	c.add(time.Minute)
	r = agyReading("4", c.t, agyPool("Gemini", 60, c.t.Add(time.Hour)))
	world.reading = &r
	e.pollKept(context.Background())
	if got := groups(); got != "Gemini=1,Claude / GPT=0" {
		t.Fatalf("after the pool is gone: %s", got)
	}
	if len(h.requests) != 0 || len(h.refreshed) != 0 {
		t.Fatalf("requests %+v, refreshes %v", h.requests, h.refreshed)
	}
}

// A pool that agy shows without a figure does not hold back the windows
// of the other pools: after an upgrade, the Gemini 5-hour window appears
// and becomes an ignition target while the Claude / GPT pool has no
// figure.
func TestAgyNewWindowBesideAnUnreadPool(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	weekly := func(remaining float64) quota.RawGroup {
		return withWeekly(quota.RawGroup{Key: "antigravity:gemini-models", SourceLabel: "Gemini"}, "Gemini", remaining, c.t.Add(6*24*time.Hour))
	}
	// A free account: Gemini has only its weekly window.
	c.add(time.Minute)
	r := agyReading("4", c.t, weekly(90), agyPool("Claude & GPT", 100, c.t.Add(time.Hour)))
	world.reading = &r
	e.pollKept(ctx)
	// After the upgrade: Gemini's 5-hour window, Claude / GPT without a
	// figure.
	c.add(time.Minute)
	upgraded := agyReading("4", c.t, withWeekly(agyPool("Gemini", 100, c.t.Add(-time.Minute)), "Gemini", 90, c.t.Add(6*24*time.Hour)))
	upgraded.Unread = []string{"antigravity:claude-and-gpt-models"}
	world.reading = &upgraded
	e.pollKept(ctx)
	gemini := e.groups["antigravity:4:antigravity:gemini-models"]
	if _, ok := gemini.fiveHour(); !ok || len(gemini.Windows) != 2 {
		t.Fatalf("Gemini %+v", gemini.Windows)
	}
	if g := e.groups["antigravity:4:antigravity:claude-and-gpt-models"]; len(g.Windows) != 1 || g.Windows[0].Remaining != 100 {
		t.Fatalf("Claude / GPT %+v", g.Windows)
	}
	if agyTarget(t, e).group.Key != gemini.Key {
		t.Fatal("the new 5-hour window is no target")
	}
}

// A scheduled round has Magpie refresh an agy-cli account whose reading is
// magpieStaleAfter old, at most once per magpieRefreshGap, a manual
// refresh included; the background worker runs the refresh.
func TestAgyStaleReadingIsRefreshedAtMostOncePerGap(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	world.refresh = func() *host.KeptQuota { return nil }
	poll := func(d time.Duration) {
		c.add(d)
		e.poll(context.Background(), e.config(), c.t, "", false)
		workAgy(e)
	}
	poll(10 * time.Minute)
	if len(h.refreshed) != 0 {
		t.Fatalf("a fresh reading was refreshed: %v", h.refreshed)
	}
	poll(5 * time.Minute)
	if len(h.refreshed) != 1 || h.confirming[0] {
		t.Fatalf("stale reading: refreshed %v", h.refreshed)
	}
	// Magpie read nothing new: the next rounds wait for the gap.
	poll(5 * time.Minute)
	poll(5 * time.Minute)
	if len(h.refreshed) != 1 {
		t.Fatalf("within the gap: refreshed %v", h.refreshed)
	}
	poll(5 * time.Minute)
	if len(h.refreshed) != 2 {
		t.Fatalf("after the gap: refreshed %v", h.refreshed)
	}
	// A manual refresh starts the gap again.
	c.add(time.Minute)
	e.manualPoll(context.Background(), e.config(), "4", time.Time{})
	workAgy(e)
	if len(h.refreshed) != 3 {
		t.Fatalf("manual refresh: refreshed %v", h.refreshed)
	}
	poll(10 * time.Minute)
	if len(h.refreshed) != 3 {
		t.Fatalf("after a manual refresh: refreshed %v", h.refreshed)
	}
	poll(5 * time.Minute)
	if len(h.refreshed) != 4 {
		t.Fatalf("after the gap of the manual refresh: refreshed %v", h.refreshed)
	}
}

// The probes around a reset only read Magpie's kept reading: they neither
// refresh an account nor mark it for a refresh, even with a stale reading.
func TestAgyProbeDoesNotRefresh(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	c.add(time.Minute)
	r := agyReading("4", c.t, agyPool("Gemini", 40, c.t.Add(20*time.Minute)))
	world.reading = &r
	e.pollKept(ctx)
	c.add(20*time.Minute + probeOffset + time.Second)
	if due, _ := e.probes(c.t); len(due) == 0 {
		t.Fatal("no probe due")
	}
	e.runProbes(ctx, e.config(), c.t)
	workAgy(e)
	if refreshCount(h) != 0 || len(e.agyStale) != 0 {
		t.Fatalf("probe: refreshed %v, stale %v", h.refreshed, e.agyStale)
	}
	e.poll(ctx, e.config(), c.t, "", false)
	workAgy(e)
	if refreshCount(h) != 1 {
		t.Fatalf("scheduled round: refreshed %v", h.refreshed)
	}
}

// The scheduled refreshes take the account whose last refresh was tried
// longest ago. Accounts whose refreshes keep failing do not hold back an
// account behind them in Magpie's list: every stale account is refreshed,
// each at most once per magpieRefreshGap.
func TestAgyScheduledRefreshesReachEveryAccount(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	h.auths = nil
	for i := 1; i <= 4; i++ {
		h.auths = append(h.auths, authEntry{ID: fmt.Sprintf("ag%d.json", i), AuthIndex: fmt.Sprintf("a%d", i), Name: fmt.Sprintf("ag%d.json", i), Provider: "agy-cli", Email: fmt.Sprintf("ag%d@example.com", i)})
	}
	var mu sync.Mutex
	readings := map[string]host.KeptQuota{}
	h.agy = func() []host.KeptQuota {
		mu.Lock()
		defer mu.Unlock()
		var out []host.KeptQuota
		for _, r := range readings {
			out = append(out, r)
		}
		return out
	}
	// agy reads only the last account; the others it never reads.
	h.onRefresh = func(pin string) {
		if pin == "a4" {
			mu.Lock()
			readings[pin] = agyReading(pin, c.t, agyPool("Gemini", 100, c.t.Add(time.Hour)))
			mu.Unlock()
		}
	}
	e, _ := newTestEngine(t, h, c)
	cfg := e.config()
	p := cfg.Provider("antigravity")
	p.Monitor = true
	cfg.Providers["antigravity"] = p
	e.Configure(cfg, nil)
	count := map[string]int{}
	last := map[string]time.Time{}
	for i := 0; i < 13; i++ {
		e.poll(context.Background(), e.config(), c.t, "", false)
		for {
			job, ok := e.nextAgyJob(c.t)
			if !ok {
				break
			}
			if at, seen := last[job.pin]; seen && c.t.Sub(at) < magpieRefreshGap {
				t.Fatalf("%s refreshed again after %v", job.pin, c.t.Sub(at))
			}
			count[job.pin]++
			last[job.pin] = c.t
			e.agyFinished(context.Background(), e.config(), agyDone{job: job, result: e.runAgyJob(context.Background(), job)})
		}
		if i == 0 && count["a4"] != 1 {
			t.Fatalf("first round: %v", count)
		}
		c.add(5 * time.Minute)
	}
	// Every 15 minutes for an hour: the failing accounts each time, the
	// last one once, as its reading is fresh afterwards.
	for _, pin := range []string{"a1", "a2", "a3"} {
		if count[pin] != 5 {
			t.Fatalf("refreshes %v", count)
		}
	}
	if count["a4"] < 1 || count["a4"] > 5 {
		t.Fatalf("refreshes %v", count)
	}
}

// An ignition does not wait for its confirmation, and the background
// worker's refreshes hold back neither the loop nor other work: while a
// refresh runs, the loop answers commands; a manual refresh of the same
// account takes the result of the refresh running.
func TestAgyRefreshesDoNotHoldTheLoop(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := agyTarget(t, e).group.Key
	sent := c.t
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	if refreshCount(h) != 0 || !e.confirmPending(key) {
		t.Fatalf("the ignition refreshed: %v", h.refreshed)
	}
	started, release := make(chan struct{}), make(chan struct{})
	h.onRefresh = func(string) {
		close(started)
		<-release
	}
	e.startAgyWorker(ctx)
	c.add(magpieConfirmDelays[0])
	e.dispatchAgy(ctx)
	<-started
	// The loop goes on while Magpie reads: a command is answered, and a
	// manual refresh of the account waits without holding the loop.
	lang := command{kind: "language", arg: "en", reply: make(chan commandReply, 1)}
	e.runCommand(ctx, lang)
	if err := (<-lang.reply).err; err != nil {
		t.Fatal(err)
	}
	refresh := command{kind: "refresh", arg: "4", reply: make(chan commandReply, 1)}
	e.runCommand(ctx, refresh)
	select {
	case reply := <-refresh.reply:
		t.Fatalf("the manual refresh did not wait: %v", reply.err)
	default:
	}
	r := agyReading("4", c.t.Add(20*time.Second), agyPool("Gemini", 99, sent.Add(5*time.Hour)))
	world.reading = &r
	close(release)
	d := <-e.agyDoneCh
	c.add(20 * time.Second)
	e.agyFinished(ctx, e.config(), d)
	if err := (<-refresh.reply).err; err != nil {
		t.Fatalf("manual refresh: %v", err)
	}
	if e.confirmPending(key) || len(eventsOf(e, "ignite")) != 1 || refreshCount(h) != 1 || !h.confirming[0] {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
}

// A manual refresh whose refresh by Magpie has not finished by the end of
// the manual refresh answers that it is queued; the result shows later.
func TestAgyManualRefreshAnswersQueued(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	old := manualRefreshBudget
	manualRefreshBudget = 50 * time.Millisecond
	t.Cleanup(func() { manualRefreshBudget = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	h.onRefresh = func(string) { <-release }
	e.startAgyWorker(ctx)
	refresh := command{kind: "refresh", arg: "4", reply: make(chan commandReply, 1)}
	e.runCommand(ctx, refresh)
	if err := (<-refresh.reply).err; !errors.Is(err, ErrRefreshQueued) {
		t.Fatalf("manual refresh: %v", err)
	}
	close(release)
	e.agyFinished(ctx, e.config(), <-e.agyDoneCh)
	if e.agyBusy != nil || len(e.agyWaiters) != 0 {
		t.Fatalf("busy %+v, waiters %v", e.agyBusy, e.agyWaiters)
	}
}

// The first refresh after the request confirms the new window.
func TestAgyIgnitionConfirmedByARefresh(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	sent := c.t
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99.5, sent.Add(5*time.Hour+time.Second)))
		return &r
	}
	e.performDue(context.Background(), e.config())
	key := agyTarget(t, e).group.Key
	if len(h.executed) != 1 || len(h.refreshed) != 0 || !e.confirmPending(key) {
		t.Fatalf("executed %d, refreshed %v", len(h.executed), h.refreshed)
	}
	// While it waits, the target shows as confirming, without a next
	// ignition, and cannot be ignited.
	e.refreshTargets(e.config())
	if view := e.Status().Targets[0]; !view.Confirming || !view.NextDue.IsZero() {
		t.Fatalf("target %+v", view)
	}
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	if len(h.refreshed) != 1 || !h.confirming[0] || len(eventsOf(e, "ignite")) != 1 || e.confirmPending(key) {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
	e.refreshTargets(e.config())
	if view := e.Status().Targets[0]; view.Confirming || e.state.Target(key).RollingReset {
		t.Fatalf("target %+v", view)
	}
}

// A window not confirmed by the first refresh is confirmed by a later one,
// at least a minute later; meanwhile the target is not ignited again.
func TestAgyIgnitionConfirmedByALaterRefresh(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	// agy is still reading: the refresh brings no new reading.
	world.refresh = func() *host.KeptQuota { return nil }
	sent := c.t
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("ignite: %v", err)
	}
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	if len(h.refreshed) != 1 || !e.confirmPending(key) || len(eventsOf(e, "ignite")) != 0 || len(eventsOf(e, "ignite_failed")) != 0 {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
	// Another ignition of the target waits for the confirmation.
	e.performDue(ctx, e.config())
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), true); !errors.Is(err, errConfirmBusy) || len(h.executed) != 1 {
		t.Fatalf("second ignition: %v, executed %d", err, len(h.executed))
	}
	c.add(30 * time.Second)
	workAgy(e)
	if len(h.refreshed) != 1 || e.nextAgyWake(c.t).IsZero() {
		t.Fatalf("before the minute: refreshed %v", h.refreshed)
	}
	c.add(31 * time.Second)
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", c.t.Add(40*time.Second), agyPool("Gemini", 99, sent.Add(5*time.Hour)))
		return &r
	}
	workAgy(e)
	if len(h.refreshed) != 2 || !h.confirming[1] || e.confirmPending(key) || len(eventsOf(e, "ignite")) != 1 {
		t.Fatalf("refreshed %v, events %+v", h.refreshed, e.Status().Events)
	}
}

// An account that ignites but whose quota agy never reads, such as one
// Google refuses for its region at agy's eligibility check: the ignition
// asks for three refreshes and is then paused by the usual rule, until the
// next daily start. No later round refreshes it for the ignition, and the
// scheduled refreshes keep to their gap.
func TestAgyIgnitionWithoutAReadingStopsAfterThreeRefreshes(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	world.refresh = func() *host.KeptQuota { return nil }
	e.performDue(ctx, e.config())
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	for i := 0; i < 2; i++ {
		c.add(61 * time.Second)
		workAgy(e)
	}
	if len(h.refreshed) != 3 || e.confirmPending(key) {
		t.Fatalf("refreshed %v %v", h.refreshed, h.confirming)
	}
	ts := e.state.Target(key)
	if !strings.Contains(ts.LastError, ignite.ErrNotConfirmed.Error()) || ts.CircuitOpenUntilEpoch == 0 || len(eventsOf(e, "ignite_paused")) != 1 {
		t.Fatalf("target %+v", ts)
	}
	for i := 0; i < 10; i++ {
		c.add(time.Minute)
		e.performDue(ctx, e.config())
		workAgy(e)
	}
	if len(h.executed) != 1 || len(h.refreshed) != 3 {
		t.Fatalf("after the pause: executed %d, refreshed %v", len(h.executed), h.refreshed)
	}
	// The scheduled rounds refresh it once per magpieRefreshGap.
	for i := 0; i < 6; i++ {
		c.add(5 * time.Minute)
		e.poll(ctx, e.config(), c.t, "", false)
		workAgy(e)
	}
	if n := len(h.refreshed) - 3; n != 2 {
		t.Fatalf("scheduled refreshes in 30 minutes: %d", n)
	}
}

// slidingReading is Magpie's reading of a 5-hour window that has not
// started: Google read it at read, and gave its reset as read plus 5
// hours; Magpie stamps it lag later, when agy has finished.
func slidingReading(read time.Time, lag time.Duration) *host.KeptQuota {
	read = read.Truncate(time.Second)
	r := agyReading("4", read.Add(lag), withWeekly(agyPool("Gemini", 100, read.Add(5*time.Hour)), "Gemini", 100, read.Add(7*24*time.Hour)))
	return &r
}

// A 5-hour window that has not started reads as reset "read time plus 5
// hours", moving with every reading. Magpie stamps a reading when agy has
// finished, and agy takes from seconds to half a minute, so the reading
// times cannot tell how far the reset moved: only the resets are
// compared. A placeholder is told as rolling, and a running window as
// fixed, however long agy took.
func TestAgyRollingIsToldByTheResets(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	c.add(time.Minute)
	world.reading = slidingReading(c.t, time.Second)
	e.pollKept(ctx)
	c.add(15 * time.Minute)
	world.reading = slidingReading(c.t, 25*time.Second)
	e.pollKept(ctx)
	if !e.state.Target(key).RollingReset {
		t.Fatal("two placeholders read 1 and 25 seconds before Magpie stamped them are not rolling")
	}
	// A running window: the same reset, read with different delays.
	start := c.t.Add(time.Minute).Truncate(time.Second)
	for i, lag := range []time.Duration{2 * time.Second, 30 * time.Second, time.Second} {
		c.add(5 * time.Minute)
		r := agyReading("4", c.t.Add(lag), agyPool("Gemini", 90, start.Add(5*time.Hour)))
		world.reading = &r
		e.pollKept(ctx)
		if e.state.Target(key).RollingReset {
			t.Fatalf("reading %d of a running window is rolling", i)
		}
	}
}

// After an ignition that did not start the window, every refresh reads a
// new placeholder, later than the request's answer plus 5 hours: none
// confirms it, however long agy took, and the ignition counts as not
// confirmed.
func TestAgySlidingWindowIsNoConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	c.add(time.Minute)
	world.reading = slidingReading(c.t.Add(-2*time.Minute), time.Second)
	e.pollKept(ctx)
	// agy reads the quota a second after the refresh starts and takes 24
	// to 30 seconds more.
	lags := []time.Duration{24 * time.Second, 30 * time.Second, 2 * time.Second}
	world.refresh = func() *host.KeptQuota {
		lag := lags[0]
		lags = lags[1:]
		return slidingReading(c.t.Add(time.Second), lag)
	}
	e.performDue(ctx, e.config())
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	for i := 0; i < 2; i++ {
		c.add(61 * time.Second)
		workAgy(e)
	}
	if len(h.executed) != 1 || len(h.refreshed) != 3 || len(eventsOf(e, "ignite")) != 0 {
		t.Fatalf("executed %d, refreshed %v, events %+v", len(h.executed), h.refreshed, e.Status().Events)
	}
	if ts := e.state.Target(key); !strings.Contains(ts.LastError, ignite.ErrNotConfirmed.Error()) {
		t.Fatalf("target %+v", ts)
	}
}

// The window the ignition started resets within its request's time plus 5
// hours: the last refresh that shows it confirms the ignition, after
// refreshes that showed placeholders, whatever the delay of Magpie's
// stamps. The confirmed window is fixed, so it is not ignited again.
func TestAgyNewWindowOnTheLastRefreshIsConfirmed(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	key := agyTarget(t, e).group.Key
	sent := c.t
	refreshes := 0
	world.refresh = func() *host.KeptQuota {
		refreshes++
		if refreshes < 3 {
			return slidingReading(c.t.Add(time.Second), 3*time.Second)
		}
		// Google started the window a second after the request was sent.
		r := agyReading("4", c.t.Add(30*time.Second), agyPool("Gemini", 99, sent.Add(5*time.Hour+time.Second).Truncate(time.Second)))
		return &r
	}
	e.performDue(ctx, e.config())
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	for i := 0; i < 2; i++ {
		c.add(61 * time.Second)
		workAgy(e)
	}
	if len(h.refreshed) != 3 || len(eventsOf(e, "ignite")) != 1 || e.state.Target(key).RollingReset {
		t.Fatalf("refreshed %v, rolling %v, events %+v", h.refreshed, e.state.Target(key).RollingReset, e.Status().Events)
	}
	c.add(2 * time.Minute)
	e.performDue(ctx, e.config())
	if len(h.executed) != 1 {
		t.Fatalf("ignited again: %d", len(h.executed))
	}
}

// After ignition finds the credential rejected, a reading of Magpie's made
// certainly after the problem was last found lets ignition try again: agy
// reads with the account's own sign-in. Magpie stamps readAt only when agy
// has finished, so a reading of the kept quota counts from magpieReadSpan
// after the problem on, and the card of a refresh Lamplighter sent after
// the problem at once. A refresh sent before the problem does not count,
// however late its card was stamped, nor does a kept reading taken between
// two failures and applied after the second.
func TestAgyReadingAfterAProblemResumesIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	ctx := context.Background()
	rejected := &host.Error{Code: "host_call_failed", Message: `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, Status: 401}
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 401}, rejected
	}
	ended := func() quota.RawGroup { return agyPool("Gemini", 100, c.t.Add(-time.Minute)) }
	c.add(2*time.Minute + 700*time.Millisecond)
	e.performDue(ctx, e.config())
	first := c.t
	if !e.credentialBlocked("4") {
		t.Fatalf("credential %+v", e.state.Credentials["4"])
	}
	e.applyKeptQuota(ctx, agyReading("4", first.Add(magpieReadSpan-time.Second), ended()), time.Time{})
	if !e.credentialBlocked("4") {
		t.Fatal("a kept reading stamped within magpieReadSpan of the failure resumed ignition")
	}
	e.applyKeptQuota(ctx, agyReading("4", first.Add(magpieReadSpan-500*time.Millisecond), ended()), first.Add(-time.Second))
	if !e.credentialBlocked("4") {
		t.Fatal("the card of a refresh sent before the failure resumed ignition")
	}
	c.add(time.Minute)
	e.applyKeptQuota(ctx, agyReading("4", first.Add(magpieReadSpan-200*time.Millisecond), ended()), first.Add(time.Millisecond))
	if e.credentialBlocked("4") {
		t.Fatalf("a refresh sent after the failure: credential %+v", e.state.Credentials["4"])
	}
	// Ignition tries again and fails again; a reading taken between the
	// two failures, applied after the second, does not resume ignition.
	c.add(4 * time.Minute)
	between := c.t.Add(-time.Minute)
	e.performDue(ctx, e.config())
	second := c.t
	if !e.credentialBlocked("4") || !e.state.Credentials["4"].FailedAt.Equal(second) || e.state.Credentials["4"].Since != first.Unix() {
		t.Fatalf("credential %+v", e.state.Credentials["4"])
	}
	e.applyKeptQuota(ctx, agyReading("4", second.Add(magpieReadSpan-time.Millisecond), ended()), between)
	if !e.credentialBlocked("4") {
		t.Fatal("a reading from between two failures resumed ignition")
	}
	e.applyKeptQuota(ctx, agyReading("4", second.Add(magpieReadSpan), ended()), time.Time{})
	if e.credentialBlocked("4") {
		t.Fatalf("a kept reading magpieReadSpan after the failure: credential %+v", e.state.Credentials["4"])
	}
}

// The background refresh in flight when ignition finds the credential
// rejected was sent before the problem: its card, stamped after the
// problem, does not resume ignition. The next refresh, sent after it,
// does.
func TestAgyRefreshSentBeforeAProblemDoesNotResumeIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	rejected := &host.Error{Code: "host_call_failed", Message: `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, Status: 401}
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 401}, rejected
	}
	// A manual refresh is sent at 10:00:04; agy read the quota before the
	// rejection, Magpie stamps the card at 10:00:20, after it.
	c.add(4 * time.Second)
	e.queueAgyManual("4")
	job, ok := e.nextAgyJob(c.t)
	if !ok {
		t.Fatal("no job")
	}
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", c.t.Add(16*time.Second), agyPool("Gemini", 100, c.t.Add(-time.Minute)))
		return &r
	}
	inFlight := agyDone{job: job, result: e.runAgyJob(ctx, job)}
	if inFlight.result.sentAt.IsZero() || inFlight.result.err != nil {
		t.Fatalf("refresh %+v", inFlight.result)
	}
	c.add(6 * time.Second)
	e.performDue(ctx, e.config())
	if !e.credentialBlocked("4") {
		t.Fatalf("credential %+v", e.state.Credentials["4"])
	}
	c.add(10 * time.Second)
	e.agyFinished(ctx, e.config(), inFlight)
	if !e.credentialBlocked("4") {
		t.Fatal("a refresh sent before the rejection resumed ignition")
	}
	c.add(time.Minute)
	e.queueAgyManual("4")
	workAgy(e)
	if e.credentialBlocked("4") {
		t.Fatalf("a refresh sent after the rejection: credential %+v", e.state.Credentials["4"])
	}
}

// State saved before failed_at existed has the problem's time to the
// second only: a refresh sent within that second does not count as after
// it.
func TestAgyLegacyProblemTimeIsTheEndOfItsSecond(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, _ := agyHost(t, c)
	ctx := context.Background()
	since := c.t.Add(time.Minute)
	e.state.Credentials = map[string]*store.CredentialState{"4": {Reason: "auth", Source: sourceIgnite, Since: since.Unix()}}
	ended := agyPool("Gemini", 100, since.Add(-time.Minute))
	e.applyKeptQuota(ctx, agyReading("4", since.Add(10*time.Second), ended), since.Add(900*time.Millisecond))
	if !e.credentialBlocked("4") {
		t.Fatal("a refresh sent within the problem's second resumed ignition")
	}
	e.applyKeptQuota(ctx, agyReading("4", since.Add(20*time.Second), ended), since.Add(time.Second))
	if e.credentialBlocked("4") {
		t.Fatalf("credential %+v", e.state.Credentials["4"])
	}
}

// A manual ignition answers ErrIgnitionPending at once; the loop records
// the outcome.
func TestAgyManualIgnitionAnswersPending(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	world.refresh = func() *host.KeptQuota { return nil }
	key := agyTarget(t, e).group.Key
	cmd := command{kind: "ignite", arg: key, reply: make(chan commandReply, 1)}
	e.runCommand(context.Background(), cmd)
	if err := (<-cmd.reply).err; !errors.Is(err, ErrIgnitionPending) || !e.confirmPending(key) || refreshCount(h) != 0 {
		t.Fatalf("manual ignition: %v", err)
	}
	if view := e.Status().Targets[0]; !view.Confirming {
		t.Fatalf("target %+v", view)
	}
}

// The plan comes from Magpie's account list, also while Magpie has no
// reading of the account.
func TestAgyPlanComesFromTheAccountList(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	world.reading = nil
	h.auths[0].Plan = "Google AI Ultra"
	c.add(5 * time.Minute)
	e.poll(context.Background(), e.config(), c.t, "", false)
	if got := agPlan(e); got != "Ultra" {
		t.Fatalf("plan %q", got)
	}
	if !slices.ContainsFunc(eventsOf(e, "plan_change"), func(ev Event) bool { return strings.Contains(ev.Message, "Pro → Ultra") }) {
		t.Fatalf("events %+v", e.Status().Events)
	}
}

// refresh_quotas answers a refresh Magpie has not finished in time as
// queued, not as a failure.
func TestAgyRefreshQuotasAnswersQueued(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	old := manualRefreshBudget
	manualRefreshBudget = 50 * time.Millisecond
	t.Cleanup(func() { manualRefreshBudget = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	h.onRefresh = func(string) { <-release }
	e.startAgyWorker(ctx)
	result := mcpRefresh(t, e, "4")
	if result["ok"] != true || result["queued"] != true || result["error"] != nil {
		t.Fatalf("refresh_quotas %#v", result)
	}
	close(release)
	e.agyFinished(ctx, e.config(), <-e.agyDoneCh)
}
