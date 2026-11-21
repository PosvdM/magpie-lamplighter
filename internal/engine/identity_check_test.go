package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// plainHost is a host.Host and nothing more: it implements none of the
// optional interfaces, such as host.IdentityChecker.
type plainHost struct{ host.Host }

// Without a host.IdentityChecker no agy-cli account is confirmed, though
// its Credential reads: neither manual nor automatic ignition sends it a
// request. Codex and Claude ignite as before.
func TestAgyIgnitionNeedsAnIdentityChecker(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	ctx := context.Background()

	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyIgnitionHost(t, c)
	e.host = plainHost{h}
	chosen := agyTarget(t, e)
	if _, err := e.host.Credential(ctx, host.Account{Pin: "4"}); err != nil {
		t.Fatalf("the test needs a credential that reads: %v", err)
	}
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
	if err := ec.igniteTarget(ctx, ec.config(), claudeTarget(t, ec), true); err != nil || len(hc.executed) != 2 {
		t.Fatalf("manual Claude: err=%v executed=%d", err, len(hc.executed))
	}
	codex := target{}
	for _, candidate := range ec.collectTargets(ec.config()) {
		if candidate.group.Provider == "codex" {
			codex = candidate
		}
	}
	if codex.cred == nil {
		t.Fatal("missing Codex target")
	}
	if err := ec.igniteTarget(ctx, ec.config(), codex, true); errors.Is(err, host.ErrIdentity) || len(hc.executed) != 3 || hc.executed[2].Account != codex.cred.AuthIndex {
		t.Fatalf("manual Codex: err=%v executed=%+v", err, hc.executed)
	}
}

// logHost records the log lines of the engine.
type logHost struct {
	*fakeHost
	mu    sync.Mutex
	lines []string
}

func (h *logHost) Log(level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, fmt.Sprint(level, " ", message, " ", fields))
}

func (h *logHost) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}

// The reason an agy-cli account is not confirmed shows no value of the
// credential file and no whole email, in the error, the target, the
// events and the log; a host.ErrIdentity reason keeps the masked email.
func TestAgyIdentityErrorShowsNoValues(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
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
			h, e, _ := agyIgnitionHost(t, c)
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

// A rejection waits for a sign-in other than the one confirmed right
// before the request that went out, which agy may have changed after the
// first confirmation: the next poll does not take it for a new one.
func TestAgyRejectionKeepsTheSignInSent(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyIgnitionHost(t, c)
	ctx := context.Background()
	const signedInAgain = `{"access_token":"t4b","refresh_token":"r4b","project_id":"p4"}`
	h.listModels = func() ([]string, error) { return []string{"gemini-3.8-flash-high", "gemini-3.5-flash"}, nil }
	h.execute = func(req host.ModelRequest) (host.ModelResponse, error) {
		if len(h.executed) == 1 {
			// agy signs in to the account again while the first candidate
			// is refused.
			h.files["4"] = signedInAgain
			return host.ModelResponse{StatusCode: 404}, &host.Error{Code: "model_execution_failed", Status: 404,
				Message: `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`}
		}
		return host.ModelResponse{StatusCode: 401}, &host.Error{Code: "model_execution_failed", Status: 401,
			Message: `{"error":{"message":"invalid authentication credentials","type":"authentication_error"}}`}
	}
	e.performDue(ctx, e.config())
	if len(h.executed) != 2 || !e.credentialBlocked("4") {
		t.Fatalf("executed %d, credential %+v", len(h.executed), e.state.Credentials["4"])
	}
	if got, want := e.state.Credentials["4"].Fingerprint, fingerprintOf([]byte(signedInAgain)); got != want {
		t.Fatalf("fingerprint %q, want that of the sign-in sent, %q", got, want)
	}
	e.poll(ctx, e.config(), c.t, "", false)
	if !e.credentialBlocked("4") {
		t.Fatal("the rejected sign-in was taken for a new one")
	}
}
