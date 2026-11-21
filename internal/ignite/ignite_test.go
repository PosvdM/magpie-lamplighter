package ignite

import (
	"errors"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

var shanghai = time.FixedZone("UTC+8", 8*3600)

func schedule() Schedule {
	return Schedule{Cfg: config.Default().Ignition, Loc: shanghai}
}

func local(hour, minute int) time.Time {
	return time.Date(2026, 10, 4, hour, minute, 0, 0, shanghai)
}

func TestWaitsForDailyStart(t *testing.T) {
	due := schedule().DueAt(&store.TargetState{}, time.Time{}, local(5, 0))
	if !due.Equal(local(7, 0)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestUsesResetPlusGrace(t *testing.T) {
	reset := local(10, 0)
	due := schedule().DueAt(&store.TargetState{}, reset, local(9, 0))
	if !due.Equal(reset.Add(3 * time.Second)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestResetAfterCutoffWaitsForNextDay(t *testing.T) {
	due := schedule().DueAt(&store.TargetState{}, local(23, 0), local(20, 0))
	if !due.Equal(local(7, 0).AddDate(0, 0, 1)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestAfterCutoffWaitsForNextDay(t *testing.T) {
	due := schedule().DueAt(&store.TargetState{}, time.Time{}, local(22, 31))
	if !due.Equal(local(7, 0).AddDate(0, 0, 1)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestExpiredResetIsDueNow(t *testing.T) {
	now := local(12, 0)
	due := schedule().DueAt(&store.TargetState{}, local(11, 59), now)
	if !due.Equal(now) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestRollingResetIsDetectedFromTwoObservations(t *testing.T) {
	ts := &store.TargetState{}
	first := local(9, 0)
	if Observe(ts, first.Add(5*time.Hour), first, false) {
		t.Fatal("one observation cannot be rolling")
	}
	second := first.Add(5 * time.Minute)
	if !Observe(ts, second.Add(5*time.Hour), second, false) {
		t.Fatal("reset moving with time must be rolling")
	}
	if due := schedule().DueAt(ts, second.Add(5*time.Hour), second); !due.Equal(second) {
		t.Fatalf("rolling window should be due now, got %v", due.In(shanghai))
	}
}

func TestFixedResetIsNotRolling(t *testing.T) {
	ts := &store.TargetState{}
	first := local(9, 0)
	reset := first.Add(5 * time.Hour)
	Observe(ts, reset, first, false)
	if Observe(ts, reset, first.Add(5*time.Minute), false) {
		t.Fatal("a fixed reset is a started window")
	}
}

func TestPassiveObservationIsNeverRolling(t *testing.T) {
	ts := &store.TargetState{}
	first := local(9, 0)
	Observe(ts, first.Add(5*time.Hour), first, false)
	second := first.Add(5 * time.Minute)
	if Observe(ts, second.Add(5*time.Hour), second, true) {
		t.Fatal("passive data comes from a real request, which starts the window")
	}
}

func TestHardFailureOpensCircuitImmediately(t *testing.T) {
	ts := &store.TargetState{}
	now := local(10, 0)
	err := &host.Error{Code: "host_call_failed", Message: "upstream rejected", Status: 429}
	outcome := schedule().RecordFailure(ts, now, err)
	if !outcome.CircuitOpened || !outcome.NotifyCircuit || !outcome.Until.Equal(local(7, 0).AddDate(0, 0, 1)) {
		t.Fatalf("outcome %+v", outcome)
	}
	again := schedule().RecordFailure(ts, now.Add(time.Minute), err)
	if again.NotifyCircuit {
		t.Fatal("the pause must be notified only once")
	}
}

func TestTransientFailureBacksOffThenOpensCircuit(t *testing.T) {
	ts := &store.TargetState{}
	now := local(10, 0)
	err := errors.New("connection reset")
	if o := schedule().RecordFailure(ts, now, err); o.CircuitOpened || o.RetryIn != 5*time.Minute {
		t.Fatalf("first failure %+v", o)
	}
	if o := schedule().RecordFailure(ts, now, err); o.CircuitOpened || o.RetryIn != 15*time.Minute {
		t.Fatalf("second failure %+v", o)
	}
	if o := schedule().RecordFailure(ts, now, err); !o.CircuitOpened {
		t.Fatalf("third failure must pause, got %+v", o)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Kind
	}{
		{&host.Error{Code: "gateway_http_error", Message: `{"type":"error","error":{"type":"invalid_request_error","message":"X-Magpie-Account: a@example.com's plan doesn't list claude-haiku-9"}}`, Status: 400}, Unsupported},
		{&host.Error{Code: "gateway_http_error", Message: `{"error":{"message":"X-Magpie-Account: no account \"codex/x\" serves this model; its accounts are a@example.com","type":"not_found_error"}}`, Status: 404}, Unsupported},
		{restError("2026-10-04T14:00:17Z"), Transient},
		{&host.Error{Code: "gateway_http_error", Message: `{"error":{"message":"X-Magpie-Account: usage cap reached: every account that serves \"x\" is held","type":"rate_limit_error"}}`, Status: 429}, Transient},
		{&host.Error{Code: "gateway_http_error", Message: `{"error":{"message":"rests until 2026-10-04T14:00:17Z","type":"rate_limit_error"}}`, Status: 429}, Hard},
		{&host.Error{Code: "host_call_failed", Message: "upstream error", Status: 401}, Hard},
		{&host.Error{Code: "host_call_failed", Message: "upstream error", Status: 502}, Transient},
		{ErrNotConfirmed, Hard},
		{errors.Join(ErrNoModel, errors.New("X-Magpie-Account: a's plan doesn't list x")), Hard},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}

func TestModelRejected(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&host.Error{Code: "model_execution_failed", Message: `{"type":"error","error":{"type":"not_found_error","message":"model: claude-haiku-5-5"}}`, Status: 404}, true},
		{&host.Error{Code: "model_execution_failed", Message: "invalid_request_error", Status: 400}, true},
		{errors.New("upstream returned HTTP 404"), true},
		{&host.Error{Code: "model_execution_failed", Message: "model not found", Status: 401}, false},
		{&host.Error{Code: "model_execution_failed", Message: "upstream error", Status: 403}, false},
		{&host.Error{Code: "model_execution_failed", Message: "rate_limit_error", Status: 429}, false},
		{&host.Error{Code: "gateway_http_error", Message: `{"type":"error","error":{"type":"invalid_request_error","message":"X-Magpie-Account: a@example.com's plan doesn't list claude-haiku-9"}}`, Status: 400}, false},
		{restError("2026-10-04T14:00:17Z"), false},
		{&host.Error{Code: "host_call_failed", Message: "upstream error", Status: 502}, false},
		// A disabled account refuses every model; it is no model rejection.
		{&host.Error{Code: "host_call_failed", Message: `{"error":{"code":400,"message":"This service has been disabled in this account.","status":"FAILED_PRECONDITION"}}`, Status: 400}, false},
		{&host.Error{Code: "host_call_failed", Message: `{"error":{"message":"Your account has been deactivated.","type":"invalid_request_error","code":"account_deactivated"}}`, Status: 400}, false},
		{ErrNotConfirmed, false},
		{errors.Join(ErrNoModel, errors.New("not found")), false},
	}
	for _, c := range cases {
		if got := ModelRejected(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}

// restError is Magpie's answer to a request pinned to an account it rests
// until the given time (internal/gateway fallback.go, pinTo), in the
// Anthropic format of /v1/messages.
func restError(until string) error {
	return &host.Error{Code: "gateway_http_error", Status: 429, Message: `{"type":"error","error":{"type":"rate_limit_error","message":"X-Magpie-Account: a@example.com rests until ` + until + ` (rate); no other account is tried in its place"}}`}
}

// cooldownError is the account resting 17 seconds past local(22, 0).
func cooldownError() error {
	return restError(local(22, 0).Add(17 * time.Second).UTC().Format(time.RFC3339))
}

func TestCooldownRetriesWhenItEnds(t *testing.T) {
	ts := &store.TargetState{}
	now := local(22, 0)
	o := schedule().RecordFailure(ts, now, cooldownError())
	if o.CircuitOpened || !o.Cooldown || o.RetryIn != 20*time.Second {
		t.Fatalf("outcome %+v", o)
	}
	if ts.ConsecutiveFailures != 0 {
		t.Fatalf("a cooldown must not count as a failure, got %d", ts.ConsecutiveFailures)
	}
	if held := schedule().HeldUntil(ts, now); !held.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("held until %v", held)
	}
}

func TestRepeatedCooldownCountsAsFailure(t *testing.T) {
	ts := &store.TargetState{}
	now := local(22, 0)
	schedule().RecordFailure(ts, now, cooldownError())
	o := schedule().RecordFailure(ts, now.Add(20*time.Second), cooldownError())
	if o.Cooldown || ts.ConsecutiveFailures != 1 || o.RetryIn != 5*time.Minute {
		t.Fatalf("second cooldown %+v, failures %d", o, ts.ConsecutiveFailures)
	}
}

func TestLongCooldownIsNotWaitedOut(t *testing.T) {
	now := local(22, 0)
	if _, ok := CooldownWait(restError(now.Add(time.Hour).UTC().Format(time.RFC3339)), now); ok {
		t.Fatal("an hour-long rest must use the normal retry")
	}
	if _, ok := CooldownWait(restError(now.Add(-time.Minute).UTC().Format(time.RFC3339)), now); ok {
		t.Fatal("a rest that is over is no wait")
	}
	if _, ok := CooldownWait(errors.New("connection reset"), now); ok {
		t.Fatal("other errors are not rests")
	}
	if wait, ok := CooldownWait(restError(now.Add(90*time.Second).Format(time.RFC3339)), now); !ok || wait != 90*time.Second {
		t.Fatalf("a rest with a zone offset: %v %v", wait, ok)
	}
}

// Magpie's held pinned accounts in the words of pinTo and cappedError
// (internal/gateway fallback.go), as the gateway answers before the
// headers: the whole fixed wording counts, not a phrase found anywhere.
func TestPinHeld(t *testing.T) {
	pinned := func(message string) error {
		return &host.Error{Code: "gateway_http_error", Status: 429, Message: `{"error":{"message":"X-Magpie-Account: ` + message + `","type":"rate_limit_error"}}`}
	}
	cases := []struct {
		err  error
		want bool
	}{
		{restError("2026-10-04T14:00:17Z"), true},
		{pinned(`usage cap reached: every account that serves \"x\" is held at the usage cap set on it in magpie — a; no other account is tried in its place`), true},
		{pinned(`spend cap reached: every key that serves \"x\" has spent the spend cap set on it in magpie — a; no other account is tried in its place`), true},
		{pinned(`usage limit reached: every account that serves \"x\" is held by magpie — a; no other account is tried in its place`), true},
		{pinned(`usage limit reached: every account that serves \"x\" has used up its allowance and is set in magpie not to spend credits — a`), true},
		// A vendor's own usage limit, and the phrases out of place.
		{&host.Error{Code: "gateway_http_error", Status: 429, Message: `{"error":{"message":"Claude AI usage limit reached|1790000000","type":"rate_limit_error"}}`}, false},
		{pinned(`a's plan doesn't list x; usage cap reached`), false},
		{pinned(`a rests until later`), false},
		{errors.New("usage cap reached: every account"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := PinHeld(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
		if c.want && (Classify(c.err) != Transient || failure.Diagnose(c.err).Reason != "") {
			t.Errorf("%v: a held account must be retried and is no rate limit", c.err)
		}
	}
}

func TestInWindow(t *testing.T) {
	s := schedule()
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{local(6, 59), false}, {local(7, 0), true}, {local(22, 30), true}, {local(22, 31), false},
	} {
		if got := s.InWindow(c.at); got != c.want {
			t.Errorf("%v: got %v want %v", c.at, got, c.want)
		}
	}
}

func overnight() Schedule {
	s := schedule()
	s.Cfg.StartHour, s.Cfg.EndHour, s.Cfg.EndGraceMinutes = 22, 2, 30
	return s
}

func TestInWindowAcrossMidnight(t *testing.T) {
	s := overnight()
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{local(21, 59), false}, {local(22, 0), true}, {local(23, 59), true},
		{local(0, 0), true}, {local(1, 0), true}, {local(2, 30), true},
		{local(2, 31), false}, {local(12, 0), false},
	} {
		if got := s.InWindow(c.at); got != c.want {
			t.Errorf("%v: got %v want %v", c.at, got, c.want)
		}
	}
}

func TestInWindowWhenGraceCrossesMidnight(t *testing.T) {
	s := schedule()
	s.Cfg.StartHour, s.Cfg.EndHour, s.Cfg.EndGraceMinutes = 7, 23, 90
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{local(0, 15), true}, {local(0, 30), true}, {local(0, 31), false}, {local(6, 59), false},
	} {
		if got := s.InWindow(c.at); got != c.want {
			t.Errorf("%v: got %v want %v", c.at, got, c.want)
		}
	}
}

func TestOvernightWindowUsesResetAfterMidnight(t *testing.T) {
	s := overnight()
	// After midnight, inside the window that started yesterday.
	due := s.DueAt(&store.TargetState{}, local(1, 30), local(1, 0))
	if !due.Equal(local(1, 30).Add(3 * time.Second)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
	// Before midnight, with the reset falling after midnight.
	due = s.DueAt(&store.TargetState{}, local(1, 0).AddDate(0, 0, 1), local(23, 0))
	if !due.Equal(local(1, 0).AddDate(0, 0, 1).Add(3 * time.Second)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestOvernightWindowWaitsAfterEnd(t *testing.T) {
	s := overnight()
	due := s.DueAt(&store.TargetState{}, time.Time{}, local(1, 0))
	if !due.Equal(local(1, 0)) {
		t.Fatalf("inside the window: due %v", due.In(shanghai))
	}
	due = s.DueAt(&store.TargetState{}, time.Time{}, local(3, 0))
	if !due.Equal(local(22, 0)) {
		t.Fatalf("after the window: due %v", due.In(shanghai))
	}
	// A reset after the window ends waits for the next start.
	due = s.DueAt(&store.TargetState{}, local(3, 0), local(1, 0))
	if !due.Equal(local(22, 0)) {
		t.Fatalf("reset after the end: due %v", due.In(shanghai))
	}
}
