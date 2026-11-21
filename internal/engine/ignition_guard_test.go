package engine

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// pinUnservedErr is Magpie's local refusal of a candidate model, which
// makes an ignition try the next candidate.
var pinUnservedErr = &host.Error{Code: "gateway_http_error", Status: 400,
	Message: `{"error":{"message":"X-Magpie-Account: me@example.com's plan doesn't list claude-haiku-4-5-20251001","type":"invalid_request_error"}}`}

// twoTargetHost is claudeIgnitionHost with the 5-hour window of Codex
// account 1 ended too, so that one round of ignitions has a second target
// after Claude.
func twoTargetHost(c *clock) *fakeHost {
	h := claudeIgnitionHost(c, nil)
	base := h.http
	h.http = func(req httpRequest) httpResponse {
		if req.URL == providers.CodexUsageURL && req.Headers.Get("Authorization") == "Bearer t1" {
			ended := c.t.Add(-time.Minute).Unix()
			return httpResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":` +
				strconv.FormatInt(ended, 10) + `}}}`)}
		}
		return base(req)
	}
	return h
}

// Settings changed while a round of automatic ignitions runs apply to the
// next request: the remaining candidate models and targets are dropped, and
// the drop is no failure.
func TestAutomaticIgnitionRechecksSettingsBeforeEachRequest(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	for _, tc := range []struct {
		name   string
		change func(c *clock, cfg *config.Config)
	}{
		{"unchanged", nil},
		{"ignition off", func(_ *clock, cfg *config.Config) { cfg.Ignition.Enabled = false }},
		{"provider off", func(_ *clock, cfg *config.Config) {
			for _, id := range []string{"claude", "codex"} {
				p := cfg.Providers[id]
				p.Ignite = false
				cfg.Providers[id] = p
			}
		}},
		{"group deselected", func(_ *clock, cfg *config.Config) {
			for _, id := range []string{"claude", "codex"} {
				p := cfg.Providers[id]
				p.Groups = []string{"Some other group"}
				cfg.Providers[id] = p
			}
		}},
		{"past the ignition hours", func(c *clock, _ *config.Config) {
			c.t = time.Date(2026, 10, 4, 22, 31, 0, 0, shanghai)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
			h := twoTargetHost(c)
			e, _ := newTestEngine(t, h, c)
			ctx := context.Background()
			e.poll(ctx, e.config(), c.t, "", false)
			key := claudeTarget(t, e).group.Key
			if n := len(e.collectTargets(e.config())); n < 2 {
				t.Fatalf("want two targets, got %d", n)
			}
			next := h.execute
			h.execute = func(req host.ModelRequest) (host.ModelResponse, error) {
				if len(h.executed) == 1 {
					if tc.change != nil {
						cfg := e.config()
						tc.change(c, &cfg)
						e.Configure(cfg, nil)
					}
					// Magpie refuses the first candidate: the next one would follow.
					return host.ModelResponse{}, pinUnservedErr
				}
				return next(req)
			}
			e.performDue(ctx, e.config())
			if tc.change == nil {
				// Without a change, the second candidate and the Codex target follow.
				if len(h.executed) < 3 || !slices.ContainsFunc(h.executed, func(r host.ModelRequest) bool { return r.Account == "1" }) {
					t.Fatalf("executed %+v", h.executed)
				}
				return
			}
			if len(h.executed) != 1 {
				t.Fatalf("requests after the change: %+v", h.executed[1:])
			}
			ts := e.state.Target(key)
			if ts.ConsecutiveFailures != 0 || ts.LastError != "" || ts.RetryAtEpoch != 0 || ts.CircuitOpenUntilEpoch != 0 {
				t.Fatalf("a dropped ignition was recorded as a failure: %+v", ts)
			}
		})
	}
}

// A manual ignition finds its target by key with ignition.enabled off and
// tries every candidate.
func TestManualIgnitionIgnoresIgnitionEnabled(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, pinUnservedErr)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Ignition.Enabled = false
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	key := claudeTarget(t, e).group.Key
	cmd := command{kind: "ignite", arg: key, reply: make(chan commandReply, 1)}
	e.runCommand(ctx, cmd)
	if err := (<-cmd.reply).err; err != nil || len(h.executed) != 2 {
		t.Fatalf("err=%v executed=%+v", err, h.executed)
	}
	cmd = command{kind: "ignite", arg: key + "x", reply: make(chan commandReply, 1)}
	e.runCommand(ctx, cmd)
	if err := (<-cmd.reply).err; !errors.Is(err, errNoTarget) {
		t.Fatalf("a key that is no exact match: %v", err)
	}
}

// keptSeqHost reports the Claude credential as expired and hands out a new
// kept reading of a rolling placeholder on every read: one minute later,
// with the reset five hours after it.
type keptSeqHost struct {
	*expiredHost
	c *clock
}

func (h *keptSeqHost) KeptQuotas(context.Context) ([]host.KeptQuota, error) {
	h.c.add(time.Minute)
	reading := keptClaude("3", h.c.t, 100)
	reading.Groups[0].Windows[0].Reset = h.c.t.Add(5 * time.Hour)
	return []host.KeptQuota{reading}, nil
}

// A reading Magpie took before the ignition, handed over after it, does not
// confirm the ignition.
func TestKeptReadingFromBeforeIgnitionDoesNotConfirm(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { confirmDelays = old }()
	for _, tc := range []struct {
		name    string
		readAt  time.Duration
		confirm bool
	}{
		{"read before", -30 * time.Second, false},
		{"read after", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
			h := claudeIgnitionHost(c, nil)
			h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
				return host.ModelResponse{StatusCode: 200, Header: http.Header{}}, nil
			}
			e, _ := newTestEngine(t, h, c)
			ctx := context.Background()
			e.poll(ctx, e.config(), c.t, "", false)
			chosen := claudeTarget(t, e)
			// The reading is newer than the last query, so it is applied.
			c.add(time.Minute)
			e.host = &expiredHost{fakeHost: h, pin: "3"}
			reading := keptClaude("3", c.t.Add(tc.readAt), 99)
			reading.Groups[0].Windows[0].Reset = c.t.Add(4 * time.Hour)
			h.kept = []host.KeptQuota{reading}
			err := e.igniteTarget(ctx, e.config(), chosen, true)
			if tc.confirm != (err == nil) || (!tc.confirm && !errors.Is(err, ignite.ErrNotConfirmed)) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// Kept readings come from Magpie, which can read a rolling placeholder with
// a query of its own: unlike response headers they do not show that the
// window started.
func TestKeptRollingPlaceholderDoesNotConfirm(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	base := h.http
	h.http = func(req httpRequest) httpResponse {
		if req.URL == providers.ClaudeUsageURL {
			return httpResponse{StatusCode: 200, Body: []byte(`{"five_hour":{"utilization":0,"resets_at":"` + c.t.Add(5*time.Hour).UTC().Format(time.RFC3339) + `"}}`)}
		}
		return base(req)
	}
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 200, Header: http.Header{}}, nil
	}
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	c.add(time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	chosen := claudeTarget(t, e)
	if !e.state.Target(chosen.group.Key).RollingReset {
		t.Fatal("setup: the window is no rolling placeholder")
	}
	e.host = &keptSeqHost{expiredHost: &expiredHost{fakeHost: h, pin: "3"}, c: c}
	if err := e.igniteTarget(ctx, e.config(), chosen, true); !errors.Is(err, ignite.ErrNotConfirmed) {
		t.Fatalf("err=%v", err)
	}
	if !e.state.Target(chosen.group.Key).RollingReset {
		t.Fatal("kept readings cleared the rolling placeholder")
	}
}

// A reading after the ignition that still carries the reset of the window
// the ignition was meant to replace does not confirm it.
func TestConfirmationKeepsTheResetBeforeIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	chosen := claudeTarget(t, e)
	key := chosen.group.Key
	reset := c.t.Add(4 * time.Hour)
	setWindow := func(readAt, reset time.Time) {
		e.mu.Lock()
		defer e.mu.Unlock()
		for i, w := range e.groups[key].Windows {
			if quota.IsFiveHour(w.Window) {
				e.groups[key].Windows[i].Reset, e.groups[key].Windows[i].ReadAt = reset, readAt
			}
		}
	}
	setWindow(c.t.Add(-time.Minute), reset)
	e.state.Target(key).RollingReset = true
	start := c.t
	base := e.ignitionBase(key, start)
	e.state.Target(key).RollingReset = false
	setWindow(start.Add(time.Second), reset)
	if e.confirmed(key, base, c.t) {
		t.Fatal("the reset from before the ignition confirmed it")
	}
	setWindow(start.Add(time.Second), reset.Add(time.Minute))
	if !e.confirmed(key, base, c.t) {
		t.Fatal("a new reset read after the ignition must confirm it")
	}
	// A window that was running before is no old window: a manual ignition
	// on it is confirmed by its reset.
	setWindow(c.t.Add(-time.Minute), reset)
	running := e.ignitionBase(key, start)
	setWindow(start.Add(time.Second), reset)
	if !running.oldReset.IsZero() || !e.confirmed(key, running, c.t) {
		t.Fatalf("running window %+v", running)
	}
}

// magpie_warmup uses the targets of automatic ignition: an account with
// only a weekly window is not ignited, so it is not listed.
func TestMagpieWarmupNeedsAFiveHourWindow(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := standardHost(c)
	next := base.http
	base.http = func(req httpRequest) httpResponse {
		if req.URL == providers.CodexUsageURL && req.Headers.Get("Authorization") == "Bearer t1" {
			return httpResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)}
		}
		return next(req)
	}
	h := &warmupHost{fakeHost: base, pins: []string{"1", "2", "3"}}
	e, _ := newTestEngine(t, base, c)
	e.host = h
	e.poll(context.Background(), e.config(), c.t, "", false)
	if got := e.Status().MagpieWarmup; !slices.Equal(got, []string{"2", "3"}) {
		t.Fatalf("warm-up %v", got)
	}
	for _, a := range h.seen {
		if a.Pin == "1" {
			t.Fatalf("an account without a 5-hour window was handed over: %+v", h.seen)
		}
	}
}

// Only the Magpie providers whose credentials can be read are monitored:
// Magpie's own antigravity accounts are left out, agy-cli ones are kept.
func TestLoadCredsTakesKnownHostProvidersOnly(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	h.auths = append(h.auths,
		authEntry{AuthIndex: "antigravity/x", Provider: "antigravity"},
		authEntry{AuthIndex: "gemini/y", Provider: "gemini"},
		authEntry{AuthIndex: "agy-cli/z", Provider: "AGY-CLI"},
	)
	e, _ := newTestEngine(t, h, c)
	creds, err := e.loadCreds(context.Background(), e.config())
	if err != nil {
		t.Fatal(err)
	}
	var pins []string
	for _, cred := range creds {
		pins = append(pins, cred.AuthIndex)
		if cred.AuthIndex == "agy-cli/z" && (cred.Provider != "antigravity" || cred.HostProvider != "agy-cli") {
			t.Fatalf("agy-cli account %+v", cred)
		}
	}
	if !slices.Equal(pins, []string{"1", "2", "3", "4", "agy-cli/z"}) {
		t.Fatalf("pins %v", pins)
	}
}
