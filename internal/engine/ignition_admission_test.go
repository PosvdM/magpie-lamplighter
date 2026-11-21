package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
)

// parseConfig parses raw as the configuration file.
func parseConfig(t *testing.T, raw string) config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// For a provider with group checkboxes, an empty groups list in the file
// selects no group, and a missing one keeps the default groups. Other
// providers ignite every group with an empty list.
func TestIgnitionGroupsEmptyAndAbsent(t *testing.T) {
	for _, tc := range []struct {
		name, raw          string
		gemini, claudeGPT  bool
		claudeMain, codexs bool
	}{
		{"absent", `{"providers":{"antigravity":{"ignite":true}}}`, true, false, true, true},
		{"empty", `{"providers":{"antigravity":{"ignite":true,"groups":[]}}}`, false, false, true, true},
		{"null", `{"providers":{"antigravity":{"ignite":true,"groups":null}}}`, true, false, true, true},
		{"both", `{"providers":{"antigravity":{"ignite":true,"groups":["Gemini","Claude / GPT"]}}}`, true, true, true, true},
		{"empty for providers without checkboxes", `{"providers":{"antigravity":{"ignite":true,"groups":[]},"claude":{"groups":[]},"codex":{"groups":[]}}}`, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseConfig(t, tc.raw)
			got := []bool{
				groupIgnitable(cfg, "antigravity", "Gemini"),
				groupIgnitable(cfg, "antigravity", "Claude / GPT"),
				groupIgnitable(cfg, "claude", "Claude"),
				groupIgnitable(cfg, "codex", "Codex"),
			}
			if want := []bool{tc.gemini, tc.claudeGPT, tc.claudeMain, tc.codexs}; !slices.Equal(got, want) {
				t.Fatalf("ignitable %v, want %v (groups %#v)", got, want, cfg.Providers["antigravity"].Groups)
			}
		})
	}
}

// antigravityTarget returns the Antigravity target of e, if any.
func antigravityTarget(e *Engine) (target, bool) {
	for _, t := range e.collectTargets(e.config()) {
		if t.group.Provider == "antigravity" {
			return t, true
		}
	}
	return target{}, false
}

// Unchecking the last Antigravity group stops its ignition: no target, no
// admission and no magpie_warmup entry.
func TestUncheckedLastGroupStopsIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := standardHost(c)
	h := &warmupHost{fakeHost: base, pins: []string{"1", "2", "3", "4"}}
	e, _ := newTestEngine(t, base, c)
	e.host = h
	configure := func(raw string) {
		cfg := parseConfig(t, raw)
		cfg.Timezone = "Asia/Shanghai"
		cfg.Ignition.Enabled = true
		e.Configure(cfg, nil)
	}
	configure(`{"providers":{"antigravity":{"ignite":true}}}`)
	e.poll(context.Background(), e.config(), c.t, "", false)
	chosen, ok := antigravityTarget(e)
	if !ok || !slices.Contains(e.Status().MagpieWarmup, "4") || !e.mayIgnite(chosen) {
		t.Fatalf("Gemini must be ignited by default: target %v, warm-up %v", ok, e.Status().MagpieWarmup)
	}
	// The page saves the unchecked last group as an empty list.
	configure(`{"providers":{"antigravity":{"ignite":true,"groups":[]}}}`)
	if _, ok := antigravityTarget(e); ok || slices.Contains(e.Status().MagpieWarmup, "4") || e.mayIgnite(chosen) {
		t.Fatalf("an unchecked group is still ignited: target %v, warm-up %v", ok, e.Status().MagpieWarmup)
	}
}

// Turning monitoring of a provider off stops its ignition at once, while
// the account and groups of the last poll are still loaded.
func TestMonitorOffStopsIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := standardHost(c)
	h := &warmupHost{fakeHost: base, pins: []string{"1", "2", "3"}}
	e, _ := newTestEngine(t, base, c)
	e.host = h
	e.poll(context.Background(), e.config(), c.t, "", false)
	chosen := claudeTarget(t, e)
	cfg := e.config()
	claude := cfg.Providers["claude"]
	claude.Monitor = false
	cfg.Providers["claude"] = claude
	e.Configure(cfg, nil)
	if e.creds["3"] == nil {
		t.Fatal("the test needs the account still loaded")
	}
	if e.mayIgnite(chosen) {
		t.Fatal("a provider that is not monitored may still ignite")
	}
	if slices.Contains(e.Status().MagpieWarmup, "3") {
		t.Fatalf("warm-up %v", e.Status().MagpieWarmup)
	}
	for _, t2 := range e.collectTargets(e.config()) {
		if t2.group.Provider == "claude" {
			t.Fatal("a provider that is not monitored is still a target")
		}
	}
}

// Monitoring turned off during a round drops the requests not sent yet.
func TestMonitorOffDuringARound(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := twoTargetHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	key := claudeTarget(t, e).group.Key
	h.execute = func(req host.ModelRequest) (host.ModelResponse, error) {
		cfg := e.config()
		for _, id := range []string{"claude", "codex"} {
			p := cfg.Providers[id]
			p.Monitor = false
			cfg.Providers[id] = p
		}
		e.Configure(cfg, nil)
		return host.ModelResponse{}, pinUnservedErr
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 1 {
		t.Fatalf("requests after the change: %+v", h.executed[1:])
	}
	if ts := e.state.Target(key); ts.ConsecutiveFailures != 0 || ts.LastError != "" {
		t.Fatalf("a dropped ignition was recorded as a failure: %+v", ts)
	}
}

// admissionHost lets a test see the context of each ignition request and
// act while the request waits for its response headers.
type admissionHost struct {
	*fakeHost
	open   func(ctx context.Context, req host.ModelRequest) (host.ModelStream, error)
	read   func()
	closed []string
}

func (h *admissionHost) OpenModelStream(ctx context.Context, req host.ModelRequest) (host.ModelStream, error) {
	h.fakeHost.mu.Lock()
	h.executed = append(h.executed, req)
	h.fakeHost.mu.Unlock()
	return h.open(ctx, req)
}

func (h *admissionHost) ReadModelStream(id string) (host.StreamChunk, error) {
	if h.read != nil {
		h.read()
	}
	return h.fakeHost.ReadModelStream(id)
}

func (h *admissionHost) CloseModelStream(id string) error {
	h.closed = append(h.closed, id)
	return nil
}

// newAdmissionEngine returns an engine with a due Claude target and its
// requests going to an admissionHost.
func newAdmissionEngine(t *testing.T) (*Engine, *admissionHost, *clock, target) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, base, c)
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(context.Background(), cfg, c.t, "", false)
	h := &admissionHost{fakeHost: base}
	e.host = h
	return e, h, c, claudeTarget(t, e)
}

// ignitionOff returns the configuration of e with ignition off.
func ignitionOff(e *Engine) config.Config {
	cfg := e.config()
	cfg.Ignition.Enabled = false
	return cfg
}

// A configuration that turns ignition off while an admitted request waits
// to be sent or for its headers withdraws the request: its context is
// cancelled with errIgnitionWithdrawn, and the ignition is no failure.
func TestConfigChangeWithdrawsAdmittedRequest(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	e, h, _, chosen := newAdmissionEngine(t)
	opened := make(chan struct{})
	changed := make(chan struct{})
	var cause error
	h.open = func(ctx context.Context, _ host.ModelRequest) (host.ModelStream, error) {
		close(opened)
		<-changed
		select {
		case <-ctx.Done():
			// A host does not send a request whose context has ended.
			cause = context.Cause(ctx)
			return host.ModelStream{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return host.ModelStream{ID: "sent", StatusCode: 200}, nil
		}
	}
	go func() {
		<-opened
		// The PATCH of the settings, from the HTTP goroutine.
		e.Configure(ignitionOff(e), nil)
		close(changed)
	}()
	err := e.igniteTarget(context.Background(), e.config(), chosen, false)
	if !errors.Is(err, errIgnitionWithdrawn) || !errors.Is(cause, errIgnitionWithdrawn) {
		t.Fatalf("err = %v, request cause = %v", err, cause)
	}
	ts := e.state.Target(chosen.group.Key)
	if ts.ConsecutiveFailures != 0 || ts.LastError != "" || ts.RetryAtEpoch != 0 {
		t.Fatalf("a withdrawn request was recorded as a failure: %+v", ts)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.admission != nil {
		t.Fatal("the admission was not ended")
	}
}

// A stream opened just as the settings change is closed and withdrawn too.
func TestConfigChangeClosesAStreamOpenedMeanwhile(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	e, h, _, chosen := newAdmissionEngine(t)
	h.open = func(context.Context, host.ModelRequest) (host.ModelStream, error) {
		e.Configure(ignitionOff(e), nil)
		return host.ModelStream{ID: "late", StatusCode: 200}, nil
	}
	err := e.igniteTarget(context.Background(), e.config(), chosen, false)
	if !errors.Is(err, errIgnitionWithdrawn) || !slices.Equal(h.closed, []string{"late"}) {
		t.Fatalf("err = %v, closed %v", err, h.closed)
	}
}

// A configuration change under which the target still qualifies, or one
// after the response headers arrived, leaves the request alone. A manual
// ignition passes no admission.
func TestConfigChangeKeepsQualifyingOrStartedRequests(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	for _, tc := range []struct {
		name   string
		manual bool
		open   func(e *Engine)
		read   func(e *Engine)
	}{
		{"qualifying", false, func(e *Engine) {
			cfg := e.config()
			cfg.NoticeThreshold = 40
			e.Configure(cfg, nil)
		}, nil},
		{"after the headers", false, nil, func(e *Engine) { e.Configure(ignitionOff(e), nil) }},
		{"manual", true, func(e *Engine) { e.Configure(ignitionOff(e), nil) }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, h, c, chosen := newAdmissionEngine(t)
			var ended error
			h.open = func(ctx context.Context, req host.ModelRequest) (host.ModelStream, error) {
				if tc.open != nil {
					tc.open(e)
				}
				ended = ctx.Err()
				resp, err := claudeIgnitionHost(c, nil).execute(req)
				return host.ModelStream{ID: "s", StatusCode: resp.StatusCode, Header: resp.Header}, err
			}
			if tc.read != nil {
				h.read = func() { tc.read(e) }
			}
			if err := e.igniteTarget(context.Background(), e.config(), chosen, tc.manual); err != nil || ended != nil {
				t.Fatalf("err = %v, request context %v", err, ended)
			}
		})
	}
}

// Confirmation counts readings taken after the request that worked was
// sent, not after the ignition started: refused candidates before it can
// take a while, and a reading in between proves nothing.
func TestConfirmationStartsAtTheSentRequest(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	chosen := claudeTarget(t, e)
	var sent []time.Time
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		sent = append(sent, c.t)
		if len(sent) == 1 {
			// The first candidate is refused a minute later; meanwhile a
			// reading with a new window arrives.
			c.add(time.Minute)
			applyHeaders(e, ctx, "claude", "3", c.t, map[string][]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.02"},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(5*time.Hour).Unix(), 10)},
			})
			c.add(time.Minute)
			return host.ModelResponse{}, pinUnservedErr
		}
		// No quota headers: confirmation queries the account, which still
		// reports the ended window.
		return host.ModelResponse{StatusCode: 200}, nil
	}
	err := e.igniteTarget(ctx, e.config(), chosen, false)
	if !errors.Is(err, ignite.ErrNotConfirmed) || len(sent) != 2 {
		t.Fatalf("err = %v, sent %v", err, sent)
	}
	if n := h.requestsTo(providers.ClaudeUsageURL); n < 2 {
		t.Fatalf("confirmation did not query the account: %d", n)
	}
}
