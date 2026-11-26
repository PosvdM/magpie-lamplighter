package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
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
	old := magpieConfirmDelays
	magpieConfirmDelays = []time.Duration{time.Millisecond, time.Minute, time.Minute}
	t.Cleanup(func() { magpieConfirmDelays = old })
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

// Lamplighter asks Google nothing for an agy-cli account: its quota and
// plan come from Magpie, and the reading is applied as an active query.
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
	// A complete reading adds the Claude / GPT pool.
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
	// A reading that is not complete leaves the missing pool as it is.
	c.add(time.Minute)
	partial := agyReading("4", c.t, agyPool("Gemini", 70, c.t.Add(time.Hour)))
	partial.Complete = false
	world.reading = &partial
	e.pollKept(context.Background())
	if got := groups(); got != "Gemini=1,Claude / GPT=1" {
		t.Fatalf("after a partial reading: %s", got)
	}
	// A complete reading without the pool keeps its group as a tombstone.
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

// A scheduled round has Magpie refresh an agy-cli account whose reading is
// magpieStaleAfter old, at most once per magpieRefreshGap.
func TestAgyStaleReadingIsRefreshedAtMostOncePerGap(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	world.refresh = func() *host.KeptQuota { return nil }
	poll := func(d time.Duration) {
		c.add(d)
		e.poll(context.Background(), e.config(), c.t, "", false)
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
	// A manual refresh is held to the same gap.
	if err := manualRefresh(e, "4"); err != nil && !strings.Contains(err.Error(), "没有读到新额度") {
		t.Fatalf("manual refresh: %v", err)
	}
	poll(5 * time.Minute)
	if len(h.refreshed) != 3 {
		t.Fatalf("after a manual refresh: refreshed %v", h.refreshed)
	}
}

// The first refresh after the request confirms the new window within the
// ignition.
func TestAgyIgnitionConfirmedByARefresh(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	world.refresh = func() *host.KeptQuota {
		c.add(10 * time.Second)
		r := agyReading("4", c.t, agyPool("Gemini", 99.5, c.t.Add(5*time.Hour-15*time.Second)))
		return &r
	}
	e.performDue(context.Background(), e.config())
	if len(h.executed) != 1 || len(h.refreshed) != 1 || !h.confirming[0] {
		t.Fatalf("executed %d, refreshed %v %v", len(h.executed), h.refreshed, h.confirming)
	}
	if len(eventsOf(e, "ignite")) != 1 || e.confirmPending(agyTarget(t, e).group.Key) {
		t.Fatalf("events %+v", e.Status().Events)
	}
}

// A window not confirmed by the first refresh is confirmed by the loop, a
// minute later, without holding back the other targets.
func TestAgyIgnitionConfirmedByTheLoop(t *testing.T) {
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
	if !e.confirmPending(key) || len(eventsOf(e, "ignite")) != 0 || len(eventsOf(e, "ignite_failed")) != 0 {
		t.Fatalf("pending %v, events %+v", e.confirmPending(key), e.Status().Events)
	}
	// Another ignition of the target waits for the confirmation.
	e.performDue(ctx, e.config())
	if err := e.igniteTarget(ctx, e.config(), agyTarget(t, e), true); !errors.Is(err, errConfirmBusy) || len(h.executed) != 1 {
		t.Fatalf("second ignition: %v, executed %d", err, len(h.executed))
	}
	c.add(30 * time.Second)
	e.runMagpieConfirms(ctx, e.config())
	if len(h.refreshed) != 1 || e.nextMagpieConfirm().IsZero() {
		t.Fatalf("before the minute: refreshed %v", h.refreshed)
	}
	c.add(31 * time.Second)
	world.refresh = func() *host.KeptQuota {
		r := agyReading("4", c.t, agyPool("Gemini", 99, sent.Add(5*time.Hour)))
		return &r
	}
	e.runMagpieConfirms(ctx, e.config())
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
	for i := 0; i < 2; i++ {
		c.add(61 * time.Second)
		e.runMagpieConfirms(ctx, e.config())
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
		e.runMagpieConfirms(ctx, e.config())
		e.performDue(ctx, e.config())
	}
	if len(h.executed) != 1 || len(h.refreshed) != 3 {
		t.Fatalf("after the pause: executed %d, refreshed %v", len(h.executed), h.refreshed)
	}
	// The scheduled rounds refresh it once per magpieRefreshGap.
	for i := 0; i < 6; i++ {
		c.add(5 * time.Minute)
		e.poll(ctx, e.config(), c.t, "", false)
	}
	if n := len(h.refreshed) - 3; n < 1 || n > 2 {
		t.Fatalf("scheduled refreshes in 30 minutes: %d", n)
	}
}

// A 5-hour window that has not started reads as reset "read time plus 5
// hours", sliding with every reading. Two readings show it as a rolling
// placeholder, and the readings after an ignition that did not start it
// do not confirm it.
func TestAgySlidingWindowIsNoConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	sliding := func() *host.KeptQuota {
		c.add(10 * time.Second)
		r := agyReading("4", c.t, withWeekly(agyPool("Gemini", 100, c.t.Add(5*time.Hour).Truncate(time.Second)), "Gemini", 100, c.t.Add(7*24*time.Hour)))
		return &r
	}
	c.add(time.Minute)
	world.reading = sliding()
	e.pollKept(ctx)
	key := agyTarget(t, e).group.Key
	if e.state.Target(key).RollingReset {
		t.Fatal("one reading cannot tell a sliding window")
	}
	c.add(15 * time.Minute)
	world.reading = sliding()
	e.pollKept(ctx)
	if !e.state.Target(key).RollingReset {
		t.Fatal("two sliding readings are a rolling placeholder")
	}
	world.refresh = sliding
	e.performDue(ctx, e.config())
	for i := 0; i < 2; i++ {
		c.add(61 * time.Second)
		e.runMagpieConfirms(ctx, e.config())
	}
	if len(h.executed) != 1 || len(h.refreshed) != 3 || len(eventsOf(e, "ignite")) != 0 {
		t.Fatalf("executed %d, refreshed %v, events %+v", len(h.executed), h.refreshed, e.Status().Events)
	}
	if ts := e.state.Target(key); !strings.Contains(ts.LastError, ignite.ErrNotConfirmed.Error()) {
		t.Fatalf("target %+v", ts)
	}
}

// After ignition finds the credential rejected, the next complete reading
// Magpie makes after the problem began lets ignition try again: agy reads
// with the account's own sign-in. A reading from before the problem does
// not.
func TestAgyReadingAfterAProblemResumesIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	ctx := context.Background()
	rejected := &host.Error{Code: "host_call_failed", Message: `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, Status: 401}
	e.host.(*fakeHost).execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 401}, rejected
	}
	before := world.reading.ReadAt
	c.add(2 * time.Minute)
	e.performDue(ctx, e.config())
	if !e.credentialBlocked("4") {
		t.Fatalf("credential %+v", e.state.Credentials["4"])
	}
	c.add(time.Minute)
	stale := agyReading("4", before.Add(time.Second), agyPool("Gemini", 100, c.t.Add(-time.Minute)))
	e.applyKeptQuota(ctx, stale)
	if !e.credentialBlocked("4") {
		t.Fatal("a reading from before the problem resumed ignition")
	}
	r := agyReading("4", c.t, agyPool("Gemini", 100, c.t.Add(-time.Minute)))
	e.applyKeptQuota(ctx, r)
	if e.credentialBlocked("4") {
		t.Fatalf("credential %+v", e.state.Credentials["4"])
	}
}

// A manual ignition whose window the first refresh does not confirm answers
// ErrIgnitionPending; the loop records the outcome.
func TestAgyManualIgnitionAnswersPending(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	_, e, world := agyHost(t, c)
	world.refresh = func() *host.KeptQuota { return nil }
	key := agyTarget(t, e).group.Key
	cmd := command{kind: "ignite", arg: key, reply: make(chan commandReply, 1)}
	e.runCommand(context.Background(), cmd)
	if err := (<-cmd.reply).err; !errors.Is(err, ErrIgnitionPending) || !e.confirmPending(key) {
		t.Fatalf("manual ignition: %v", err)
	}
}
