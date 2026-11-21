package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// agySwitched is what the host returns for agy's own account after agy
// signed in to another Google account.
var agySwitched = fmt.Errorf("%w（***zz）：agy 已改登其他账号", host.ErrIdentity)

// agyIgnitionHost serves the agy-cli account "4" with an ended 5-hour
// window, and only Antigravity ignites.
func agyIgnitionHost(t *testing.T, c *clock) (*fakeHost, *Engine, *fakeSender) {
	t.Helper()
	h := standardHost(c)
	h.files["4"] = `{"access_token":"t4","refresh_token":"r4","project_id":"p4"}`
	base := h.http
	h.http = func(req httpRequest) httpResponse {
		if strings.Contains(req.URL, "retrieveUserQuotaSummary") {
			ended := c.t.Add(-time.Minute).UTC().Format(time.RFC3339)
			return httpResponse{StatusCode: 200, Body: []byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"window":"5h","remainingFraction":1,"resetTime":"` + ended + `"}]}]}`)}
		}
		return base(req)
	}
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 200}, nil
	}
	e, sender := newTestEngine(t, h, c)
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	cfg.Providers["claude"] = config.Provider{Monitor: true}
	p := cfg.Provider("antigravity")
	p.Monitor, p.Ignite = true, true
	cfg.Providers["antigravity"] = p
	e.Configure(cfg, nil)
	e.poll(context.Background(), e.config(), c.t, "", false)
	return h, e, sender
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

// Lamplighter read the quota of agy's own account A; then agy signed in to
// B. The gateway would send the ignition with B, so automatic ignition
// sends nothing and waits like for a rejected credential: no retry, no
// circuit, no rate limit.
func TestAutomaticIgnitionSkipsAgyAccountSignedInElsewhere(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyIgnitionHost(t, c)
	ctx := context.Background()
	chosen := agyTarget(t, e)
	h.credErrs = map[string]error{"4": agySwitched}

	e.performDue(ctx, e.config())
	if len(h.executed) != 0 {
		t.Fatalf("no request may go out, got %+v", h.executed)
	}
	ts := e.state.Target(chosen.group.Key)
	if ts.ConsecutiveFailures != 1 || ts.RetryAtEpoch != 0 || ts.CircuitOpenUntilEpoch != 0 ||
		!strings.Contains(ts.LastError, "agy 已改登其他账号") || !strings.Contains(ts.LastError, "没有发送点火请求") {
		t.Fatalf("target state %+v", ts)
	}
	if !e.credentialBlocked("4") || e.credentialProblem("4") != failure.ReasonAuth {
		t.Fatalf("credential state %+v", e.state.Credentials["4"])
	}
	invalid := eventsOf(e, "credential_invalid")
	if len(invalid) != 1 || invalid[0].Params["reason"] != failure.ReasonAuth || invalid[0].Params["source"] != sourceIgnite ||
		!strings.Contains(invalid[0].Params["summary"], "agy 已改登其他账号") {
		t.Fatalf("credential events %+v", invalid)
	}
	paused := eventsOf(e, "ignite_paused")
	if len(paused) != 1 || paused[0].Params["until"] != "" || paused[0].Params["reason"] != failure.ReasonAuth {
		t.Fatalf("paused events %+v", paused)
	}
	if got := e.Status().Accounts[3].CredentialProblem; got != failure.ReasonAuth {
		t.Fatalf("status credential problem %q", got)
	}

	// Still B: the next due time sends nothing either.
	c.add(time.Hour)
	e.performDue(ctx, e.config())
	if len(h.executed) != 0 {
		t.Fatalf("no request while agy is signed in to another account, got %d", len(h.executed))
	}

	// Back to A: the readable credential counts as changed, and ignition
	// sends its request.
	h.credErrs = nil
	e.poll(ctx, e.config(), c.t, "", false)
	if e.credentialBlocked("4") {
		t.Fatal("a readable credential must end the wait")
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 1 || h.executed[0].Account != "4" {
		t.Fatalf("want one ignition of the agy-cli account, got %+v", h.executed)
	}
}

// A manual ignition reads an agy-cli account's credential too, and sends
// nothing when the account cannot be confirmed. Like every manual failure,
// it leaves the automatic and credential state alone.
func TestManualIgnitionSkipsUnconfirmedAgyAccount(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, sender := agyIgnitionHost(t, c)
	chosen := agyTarget(t, e)
	h.credErrs = map[string]error{"4": fmt.Errorf("%w：钥匙串", host.ErrIdentity)}
	before := *e.state.Target(chosen.group.Key)

	err := e.igniteTarget(context.Background(), e.config(), chosen, true)
	if !errors.Is(err, host.ErrIdentity) || len(h.executed) != 0 {
		t.Fatalf("err=%v executed=%d", err, len(h.executed))
	}
	failed := eventsOf(e, "ignite_failed")
	if len(failed) != 1 || failed[0].Params["manual"] != "1" || !strings.Contains(failed[0].Detail, "钥匙串") {
		t.Fatalf("manual events %+v", failed)
	}
	after := *e.state.Target(chosen.group.Key)
	after.LastAttemptEpoch = before.LastAttemptEpoch
	if after.ConsecutiveFailures != 0 || after.LastError != "" || e.state.Credentials["4"] != nil || len(sender.sent) != 0 {
		t.Fatalf("manual failure changed state: target %+v credential %+v", after, e.state.Credentials["4"])
	}
}

// Codex and Claude requests use the account's own credential, so manual
// ignition does not read it first.
func TestManualIgnitionOfClaudeDoesNotReadTheCredential(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	chosen := claudeTarget(t, e)
	h.credErrs = map[string]error{"3": agySwitched}
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); errors.Is(err, host.ErrIdentity) {
		t.Fatalf("err = %v", err)
	}
	if len(h.executed) != 1 {
		t.Fatalf("want one Claude request, got %d", len(h.executed))
	}
}

// Any error that leaves an agy-cli account unconfirmed sends no request,
// such as a proxy setting that cannot be read before the sign-in is: an
// unreadable credential could hide a sign-in of another account. The
// first readable credential ends the wait.
func TestAgyIgnitionFailsClosed(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyIgnitionHost(t, c)
	ctx := context.Background()
	chosen := agyTarget(t, e)
	h.credErrs = map[string]error{"4": errors.New("settings.json: unknown Magpie format")}

	if err := e.igniteTarget(ctx, e.config(), chosen, true); !errors.Is(err, host.ErrIdentity) || len(h.executed) != 0 {
		t.Fatalf("manual: err=%v executed=%d", err, len(h.executed))
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 0 {
		t.Fatalf("automatic: no request may go out, got %+v", h.executed)
	}
	ts := e.state.Target(chosen.group.Key)
	if !strings.Contains(ts.LastError, "没有发送点火请求") || !strings.Contains(ts.LastError, "无法确认账号身份") || !e.credentialBlocked("4") {
		t.Fatalf("target %+v credential %+v", ts, e.state.Credentials["4"])
	}

	h.credErrs = nil
	e.poll(ctx, e.config(), c.t, "", false)
	if e.credentialBlocked("4") {
		t.Fatal("a readable credential must end the wait")
	}
}

// agy can sign in to another account while a candidate model is refused;
// the account is confirmed again before the next candidate is sent, by
// automatic and manual ignition alike.
func TestAgyIdentityConfirmedBeforeEachCandidate(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	refused := &host.Error{Code: "model_execution_failed", Status: 404,
		Message: `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`}
	for _, manual := range []bool{false, true} {
		c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
		h, e, _ := agyIgnitionHost(t, c)
		ctx := context.Background()
		chosen := agyTarget(t, e)
		h.listModels = func() ([]string, error) { return []string{"gemini-3.8-flash-high", "gemini-3.5-flash"}, nil }
		h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
			// agy signs in to another account while the first candidate
			// is refused.
			h.credErrs = map[string]error{"4": agySwitched}
			return host.ModelResponse{StatusCode: 404}, refused
		}
		err := e.igniteTarget(ctx, e.config(), chosen, manual)
		if !errors.Is(err, host.ErrIdentity) || len(h.executed) != 1 || h.executed[0].Model != "gemini-3.8-flash-high" {
			t.Fatalf("manual=%v: err=%v executed=%+v", manual, err, h.executed)
		}
		// The first candidate went out; only the second was not sent.
		shown := []string{err.Error()}
		for _, ev := range e.events {
			shown = append(shown, ev.Detail, ev.Message)
		}
		if !manual {
			shown = append(shown, e.state.Target(chosen.group.Key).LastError)
		}
		for _, s := range shown {
			if strings.Contains(s, errIdentityUnsent) {
				t.Fatalf("manual=%v: %q says no request was sent", manual, s)
			}
		}
		if !strings.Contains(err.Error(), "第 2 个候选模型未发送") {
			t.Fatalf("manual=%v: err = %v", manual, err)
		}
		if manual {
			continue
		}
		if !e.credentialBlocked("4") {
			t.Fatalf("credential %+v", e.state.Credentials["4"])
		}
		// Back to the account: the first readable credential ends the wait.
		h.credErrs = nil
		e.poll(ctx, e.config(), c.t, "", false)
		if e.credentialBlocked("4") {
			t.Fatal("a readable credential must end the wait")
		}
	}
}

// checkerHost confirms identities with confirm (host.IdentityChecker).
type checkerHost struct {
	*fakeHost
	confirm func() (string, error)
	calls   int
}

func (h *checkerHost) ConfirmIdentity(context.Context, host.Account) (string, error) {
	h.calls++
	return h.confirm()
}

// A host.IdentityChecker confirms the account in place of Credential,
// which can fail for reasons other than the sign-in, such as the proxy.
func TestIgnitionAsksTheIdentityChecker(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyIgnitionHost(t, c)
	chosen := agyTarget(t, e)
	checker := &checkerHost{fakeHost: h, confirm: func() (string, error) { return "fp", nil }}
	e.host = checker
	h.credErrs = map[string]error{"4": errors.New("settings.json: unknown Magpie format")}
	_ = e.igniteTarget(context.Background(), e.config(), chosen, true)
	// Once before the model list, once before the request.
	if len(h.executed) != 1 || checker.calls != 2 {
		t.Fatalf("executed %d, confirmations %d", len(h.executed), checker.calls)
	}
	checker.confirm = func() (string, error) { return "", agySwitched }
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); !errors.Is(err, host.ErrIdentity) || len(h.executed) != 1 {
		t.Fatalf("err=%v executed=%d", err, len(h.executed))
	}
}
