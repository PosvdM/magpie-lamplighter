package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// agySwitched is what the host returns for agy's own account after agy
// signed in to another Google account.
var agySwitched = fmt.Errorf("%w（***zz）：agy 已改登其他账号", host.ErrIdentity)

// plainHost is a host.Host and nothing more: it implements none of the
// optional interfaces, such as host.IdentityChecker.
type plainHost struct{ host.Host }

// rejected401 is the gateway's error of a request the provider rejected
// for its credential.
var rejected401 = &host.Error{Code: "host_call_failed", Status: 401,
	Message: `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`}

// Lamplighter read the quota of agy's own account A; then agy signed in to
// B. The gateway would send the ignition with B, so automatic ignition
// sends nothing and waits like for a rejected credential: no retry, no
// circuit, no rate limit. Once agy is back on A, the next query reads the
// sign-in again, and ignition sends its request.
func TestAutomaticIgnitionSkipsAgyAccountSignedInElsewhere(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
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

	// Still B: neither a new reading of Magpie's nor the next due time
	// sends anything.
	c.add(time.Hour)
	e.poll(ctx, e.config(), c.t, "", false)
	e.performDue(ctx, e.config())
	if len(h.executed) != 0 || !e.credentialBlocked("4") {
		t.Fatalf("no request while agy is signed in to another account, got %d", len(h.executed))
	}

	// Back to A: the confirmed sign-in counts as changed, and ignition
	// sends its request.
	h.credErrs = nil
	c.add(time.Hour)
	e.poll(ctx, e.config(), c.t, "", false)
	if e.credentialBlocked("4") {
		t.Fatal("a confirmed sign-in must end the wait")
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 1 || h.executed[0].Account != "4" {
		t.Fatalf("want one ignition of the agy-cli account, got %+v", h.executed)
	}
}

// A manual ignition confirms an agy-cli account too, and sends nothing
// when the account cannot be confirmed. Like every manual failure, it
// leaves the automatic and credential state alone.
func TestManualIgnitionSkipsUnconfirmedAgyAccount(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
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
	if after.ConsecutiveFailures != 0 || after.LastError != "" || e.state.Credentials["4"] != nil || e.confirmPending(chosen.group.Key) {
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

// Without a host.IdentityChecker no agy-cli account is confirmed: neither
// manual nor automatic ignition sends it a request. Claude ignites as
// before.
func TestAgyIgnitionNeedsAnIdentityChecker(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	e.host = plainHost{h}
	chosen := agyTarget(t, e)
	err := e.igniteTarget(ctx, e.config(), chosen, true)
	if !errors.Is(err, host.ErrIdentity) || !strings.Contains(err.Error(), "宿主不支持核对") || len(h.executed) != 0 {
		t.Fatalf("manual: err=%v executed=%d", err, len(h.executed))
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 0 {
		t.Fatalf("automatic: no request may go out, got %+v", h.executed)
	}
	if ts := e.state.Target(chosen.group.Key); !strings.Contains(ts.LastError, "没有发送点火请求") || !e.credentialBlocked("4") {
		t.Fatalf("target %+v credential %+v", ts, e.state.Credentials["4"])
	}

	c = &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	hc := claudeIgnitionHost(c, nil)
	ec, _ := newTestEngine(t, hc, c)
	ec.host = plainHost{hc}
	ec.poll(ctx, ec.config(), c.t, "", false)
	ec.performDue(ctx, ec.config())
	if len(hc.executed) != 1 || hc.executed[0].Account != "3" {
		t.Fatalf("automatic Claude: %+v", hc.executed)
	}
}

// The reason an agy-cli account is not confirmed shows no value of the
// credential file and no whole email, in the error, the target, the
// events and the log; a host.ErrIdentity reason keeps the masked email.
func TestAgyIdentityErrorShowsNoValues(t *testing.T) {
	const value = "ya29.leaked-expiry-value"
	for _, tc := range []struct {
		name   string
		err    error
		want   string
		hidden []string
	}{
		{"format", fmt.Errorf("agy 的凭证文件：%w: expiry %q is not RFC 3339", host.ErrFormat, value), "无法确认账号身份：凭证文件格式不对", []string{value, "ya29"}},
		{"unreadable", fmt.Errorf("open /home/someone.else@example.org/token: permission denied"), "无法确认账号身份：读取凭证失败", []string{"someone.else@example.org"}},
		{"identity", fmt.Errorf("%w：agy 登录的是 someone.else@example.org，token %q", host.ErrIdentity, value), "agy 登录的是 ***se", []string{"someone.else@example.org", value}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
			h, e, _ := agyHost(t, c)
			logs := &logHost{fakeHost: h}
			e.host = logs
			chosen := agyTarget(t, e)
			h.credErrs = map[string]error{"4": tc.err}
			ctx := context.Background()

			err := e.igniteTarget(ctx, e.config(), chosen, true)
			e.performDue(ctx, e.config())
			if !errors.Is(err, host.ErrIdentity) || len(h.executed) != 0 {
				t.Fatalf("err=%v executed=%d", err, len(h.executed))
			}
			ts := e.state.Target(chosen.group.Key)
			shown := []string{err.Error(), ts.LastError, logs.text()}
			for _, ev := range e.events {
				shown = append(shown, ev.Detail, ev.Message)
				for _, v := range ev.Params {
					shown = append(shown, v)
				}
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(ts.LastError, tc.want) {
				t.Fatalf("err %q, target %q; want %q", err, ts.LastError, tc.want)
			}
			for _, s := range shown {
				for _, hidden := range tc.hidden {
					if strings.Contains(s, hidden) {
						t.Fatalf("%q shows %q", s, hidden)
					}
				}
			}
			if !strings.Contains(logs.text(), "无法确认账号身份") {
				t.Fatalf("no debug log of the reason: %s", logs.text())
			}
		})
	}
}

// Any error that leaves an agy-cli account unconfirmed sends no request:
// an unreadable sign-in could hide a sign-in of another account. The
// first confirmed sign-in ends the wait.
func TestAgyIgnitionFailsClosed(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	ctx := context.Background()
	chosen := agyTarget(t, e)
	h.credErrs = map[string]error{"4": errors.New("plugin-auth: unknown Magpie format")}

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
	c.add(time.Hour)
	e.poll(ctx, e.config(), c.t, "", false)
	if e.credentialBlocked("4") {
		t.Fatal("a confirmed sign-in must end the wait")
	}
}

// agy can sign in to another account while a candidate model is refused;
// the account is confirmed again before the next candidate is sent, by
// automatic and manual ignition alike.
func TestAgyIdentityConfirmedBeforeEachCandidate(t *testing.T) {
	refused := &host.Error{Code: "model_execution_failed", Status: 404,
		Message: `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`}
	for _, manual := range []bool{false, true} {
		c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
		h, e, _ := agyHost(t, c)
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
		// Back to the account: the first confirmed sign-in ends the wait.
		h.credErrs = nil
		c.add(time.Hour)
		e.poll(ctx, e.config(), c.t, "", false)
		if e.credentialBlocked("4") {
			t.Fatal("a confirmed sign-in must end the wait")
		}
	}
}

// A rejection waits for a sign-in other than the one confirmed right
// before the request that went out, which agy may have changed after the
// first confirmation: the next query does not take it for a new one.
func TestAgyRejectionKeepsTheSignInSent(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	ctx := context.Background()
	const signedInAgain = `{"access_token":"t4b","refresh_token":"r4b"}`
	h.listModels = func() ([]string, error) { return []string{"gemini-3.8-flash-high", "gemini-3.5-flash"}, nil }
	h.execute = func(req host.ModelRequest) (host.ModelResponse, error) {
		if len(h.executed) == 1 {
			// agy signs in to the account again while the first candidate
			// is refused.
			h.files["4"] = signedInAgain
			return host.ModelResponse{StatusCode: 404}, &host.Error{Code: "model_execution_failed", Status: 404,
				Message: `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`}
		}
		return host.ModelResponse{StatusCode: 401}, rejected401
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 2 || !e.credentialBlocked("4") {
		t.Fatalf("executed %d, credential %+v", len(h.executed), e.state.Credentials["4"])
	}
	if got, want := e.state.Credentials["4"].Fingerprint, fingerprintOf([]byte(signedInAgain)); got != want {
		t.Fatalf("fingerprint %q, want that of the sign-in sent, %q", got, want)
	}
	c.add(time.Hour)
	e.poll(ctx, e.config(), c.t, "", false)
	if !e.credentialBlocked("4") {
		t.Fatal("the rejected sign-in was taken for a new one")
	}
}

// checkerHost confirms identities with confirm (host.IdentityChecker).
type checkerHost struct {
	*agyCards
	confirm func() (string, error)
	calls   int
}

func (h *checkerHost) ConfirmIdentity(context.Context, host.Account) (string, error) {
	h.calls++
	return h.confirm()
}

// A host.IdentityChecker confirms the account in place of Credential.
func TestIgnitionAsksTheIdentityChecker(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyHost(t, c)
	chosen := agyTarget(t, e)
	checker := &checkerHost{agyCards: e.host.(*agyCards), confirm: func() (string, error) { return "fp", nil }}
	e.host = checker
	h.credErrs = map[string]error{"4": errors.New("Credential is not asked")}
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("err = %v", err)
	}
	// Once before the model list, once before the request.
	if len(h.executed) != 1 || checker.calls != 2 {
		t.Fatalf("executed %d, confirmations %d", len(h.executed), checker.calls)
	}
	delete(e.magpieConfirms, chosen.group.Key)
	checker.confirm = func() (string, error) { return "", agySwitched }
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); !errors.Is(err, host.ErrIdentity) || len(h.executed) != 1 {
		t.Fatalf("err=%v executed=%d", err, len(h.executed))
	}
}

// After ignition finds the credential rejected, only a new sign-in lets
// ignition try again: Magpie's new readings do not, as agy can read the
// quota while Google rejects the request. Each query reads the sign-in
// from local files (noteSignIn), as the quota itself comes from Magpie.
func TestAgyNewSignInResumesIgnition(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 401}, rejected401
	}
	e.performDue(ctx, e.config())
	if !e.credentialBlocked("4") || len(h.executed) != 1 {
		t.Fatalf("executed %d, credential %+v", len(h.executed), e.state.Credentials["4"])
	}
	for i := 0; i < 3; i++ {
		c.add(10 * time.Minute)
		r := agyReading("4", c.t, agyPool("Gemini", 100, c.t.Add(-time.Minute)))
		world.reading = &r
		e.poll(ctx, e.config(), c.t, "", false)
		e.queueAgyManual("4")
		workAgy(e)
		e.performDue(ctx, e.config())
	}
	if !e.credentialBlocked("4") || len(h.executed) != 1 {
		t.Fatalf("new readings resumed ignition: executed %d, credential %+v", len(h.executed), e.state.Credentials["4"])
	}
	h.files["4"] = `{"access_token":"t4b","refresh_token":"r4b"}`
	c.add(10 * time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	if e.credentialBlocked("4") {
		t.Fatalf("a new sign-in: credential %+v", e.state.Credentials["4"])
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 2 {
		t.Fatalf("executed %d after a new sign-in", len(h.executed))
	}
}

// Two groups of an agy-cli account ignite one after the other: the first
// waits for its confirmation when the second finds the credential
// rejected. The first one's confirmation does not end the problem found
// after its request was sent; a manual ignition sent after the problem
// does.
func TestAgyEarlierConfirmationKeepsALaterProblem(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, world := agyHost(t, c)
	ctx := context.Background()
	cfg := e.config()
	p := cfg.Provider("antigravity")
	p.Groups = []string{"Gemini", "Claude / GPT"}
	cfg.Providers["antigravity"] = p
	e.Configure(cfg, nil)
	c.add(time.Minute)
	ended := c.t.Add(-time.Minute)
	r := agyReading("4", c.t, agyPool("Gemini", 100, ended), agyPool("Claude & GPT", 100, ended))
	world.reading = &r
	e.pollKept(ctx)
	var gemini, other target
	for _, candidate := range e.collectTargets(e.config()) {
		if strings.Contains(candidate.group.Key, "gemini") {
			gemini = candidate
		} else {
			other = candidate
		}
	}
	if gemini.cred == nil || other.cred == nil {
		t.Fatalf("targets %+v", e.collectTargets(e.config()))
	}
	sentA := c.t
	if err := e.igniteTarget(ctx, e.config(), gemini, false); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("first ignition: %v", err)
	}
	c.add(time.Second)
	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 401}, rejected401
	}
	if err := e.igniteTarget(ctx, e.config(), other, false); errors.Is(err, ErrIgnitionPending) || !e.credentialBlocked("4") {
		t.Fatalf("second ignition: %v, credential %+v", err, e.state.Credentials["4"])
	}
	world.refresh = func() *host.KeptQuota {
		next := agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99, sentA.Add(5*time.Hour)), agyPool("Claude & GPT", 100, ended))
		return &next
	}
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	if e.confirmPending(gemini.group.Key) || len(eventsOf(e, "ignite")) != 1 {
		t.Fatalf("first ignition not confirmed: events %+v", e.Status().Events)
	}
	if !e.credentialBlocked("4") || len(eventsOf(e, "credential_restored")) != 0 {
		t.Fatalf("an earlier request ended a later problem: credential %+v", e.state.Credentials["4"])
	}

	h.execute = func(host.ModelRequest) (host.ModelResponse, error) {
		return host.ModelResponse{StatusCode: 200}, nil
	}
	c.add(time.Minute)
	sentB := c.t
	if err := e.igniteTarget(ctx, e.config(), other, true); !errors.Is(err, ErrIgnitionPending) {
		t.Fatalf("manual ignition: %v", err)
	}
	world.refresh = func() *host.KeptQuota {
		next := agyReading("4", c.t.Add(10*time.Second), agyPool("Gemini", 99, sentA.Add(5*time.Hour)), agyPool("Claude & GPT", 99, sentB.Add(5*time.Hour)))
		return &next
	}
	c.add(magpieConfirmDelays[0])
	workAgy(e)
	if e.confirmPending(other.group.Key) || e.credentialBlocked("4") || len(eventsOf(e, "credential_restored")) != 1 {
		t.Fatalf("a later manual ignition: credential %+v, events %+v", e.state.Credentials["4"], e.Status().Events)
	}
}
