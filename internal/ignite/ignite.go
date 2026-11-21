// Package ignite decides when to start the next 5-hour quota window and keeps
// the failure protection state.
//
// A 5-hour window starts with the first request after a reset. Lamplighter
// sends a minimal request at reset plus a grace period, inside a daily time
// window, so that the next window starts right away.
package ignite

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// Some providers report an unstarted 5-hour window as a rolling placeholder:
// the reset time stays about 5 hours ahead and moves forward with every
// observation. A started window has a fixed reset time. Two observations tell
// them apart.
const (
	rollingWindow            = 5 * time.Hour
	rollingOffsetTolerance   = 120 * time.Second
	rollingMinObservation    = 2 * time.Second
	rollingDriftTolerance    = 3 * time.Second
	rollingMaxDriftTolerance = 10 * time.Second
)

// Schedule computes due times from the ignition settings.
type Schedule struct {
	Cfg config.Ignition
	Loc *time.Location
}

func (s Schedule) bounds(now time.Time) (localNow, start, end time.Time) {
	localNow = now.In(s.Loc)
	start = time.Date(localNow.Year(), localNow.Month(), localNow.Day(), s.Cfg.StartHour, 0, 0, 0, s.Loc)
	end = time.Date(localNow.Year(), localNow.Month(), localNow.Day(), s.Cfg.EndHour, 0, 0, 0, s.Loc).
		Add(time.Duration(s.Cfg.EndGraceMinutes) * time.Minute)
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	return localNow, start, end
}

// NextDailyStart returns the next start_hour after now.
func (s Schedule) NextDailyStart(now time.Time) time.Time {
	localNow := now.In(s.Loc)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), s.Cfg.StartHour, 0, 0, 0, s.Loc)
	if !localNow.Before(start) {
		start = start.AddDate(0, 0, 1)
	}
	return start.UTC()
}

func epochTime(epoch float64) time.Time {
	if epoch <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(epoch)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

func epoch(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// InWindow reports whether now lies within the daily ignition hours, from
// start_hour to end_hour plus end_grace_minutes.
func (s Schedule) InWindow(now time.Time) bool {
	localNow, start, end := s.bounds(now)
	return !localNow.Before(start) && !localNow.After(end)
}

// HeldUntil returns the end of the post-success hold or of the retry delay,
// or the zero time when the target is not held.
func (s Schedule) HeldUntil(ts *store.TargetState, now time.Time) time.Time {
	if ts.LastSuccessEpoch > 0 {
		hold := epochTime(ts.LastSuccessEpoch).Add(time.Duration(s.Cfg.PostSuccessHoldSeconds) * time.Second)
		if now.Before(hold) {
			return hold
		}
	}
	if retry := epochTime(ts.RetryAtEpoch); retry.After(now) {
		return retry
	}
	return time.Time{}
}

// DueAt returns when the target should be ignited next. reset is the current
// 5-hour reset time, or zero when unknown.
func (s Schedule) DueAt(ts *store.TargetState, reset, now time.Time) time.Time {
	localNow, start, end := s.bounds(now)
	if localNow.Before(start) {
		return start.UTC()
	}
	if localNow.After(end) {
		return s.NextDailyStart(now)
	}
	hold := s.HeldUntil(ts, now)
	if ts.RollingReset {
		if !hold.IsZero() {
			return hold
		}
		return now
	}
	if !reset.IsZero() && reset.After(now) {
		target := reset.Add(time.Duration(s.Cfg.GraceSeconds) * time.Second)
		targetLocal := target.In(s.Loc)
		sameDay := targetLocal.Year() == localNow.Year() && targetLocal.YearDay() == localNow.YearDay()
		if sameDay && !targetLocal.Before(start) && !targetLocal.After(end) {
			return target.UTC()
		}
		return s.NextDailyStart(now)
	}
	if !hold.IsZero() {
		return hold
	}
	return now
}

// Observe records one 5-hour reset observation and returns whether the
// window is a rolling placeholder. A fixed future reset clears the failure
// state, because it shows that the window is running.
func Observe(ts *store.TargetState, reset, now time.Time, passive bool) bool {
	rolling := false
	var resetEpoch *float64
	if !reset.IsZero() {
		value := epoch(reset)
		resetEpoch = &value
		prev := ts.ResetObservation
		offset := reset.Sub(now)
		// Passive observations come from a real request, which always starts
		// the window, so they are never rolling.
		if !passive && prev != nil && prev.ResetEpoch != nil && absDuration(offset-rollingWindow) <= rollingOffsetTolerance {
			elapsed := time.Duration((epoch(now) - prev.SeenEpoch) * float64(time.Second))
			shift := time.Duration((value - *prev.ResetEpoch) * float64(time.Second))
			if elapsed >= rollingMinObservation {
				tolerance := time.Duration(float64(elapsed) * 0.02)
				if tolerance < rollingDriftTolerance {
					tolerance = rollingDriftTolerance
				}
				if tolerance > rollingMaxDriftTolerance {
					tolerance = rollingMaxDriftTolerance
				}
				rolling = absDuration(shift-elapsed) <= tolerance
			}
		}
	}
	ts.RollingReset = rolling
	if !rolling && !reset.IsZero() && reset.After(now) {
		ClearFailures(ts)
	}
	ts.ResetObservation = &store.Observation{SeenEpoch: epoch(now), ResetEpoch: resetEpoch}
	return rolling
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ClearFailures resets the failure counter and closes the circuit.
func ClearFailures(ts *store.TargetState) {
	ts.ConsecutiveFailures = 0
	ts.RetryAtEpoch = 0
	ts.CircuitOpenUntilEpoch = 0
	ts.CircuitReason = ""
	ts.CircuitNotifiedUntilEpoch = 0
}

// RecordSuccess stores a confirmed ignition.
func RecordSuccess(ts *store.TargetState, model string, now time.Time) {
	ts.LastSuccessEpoch = epoch(now)
	ts.LastModel = model
	ts.LastError = ""
	ClearFailures(ts)
}

// FailureOutcome describes what RecordFailure decided.
type FailureOutcome struct {
	CircuitOpened bool
	// NotifyCircuit is true the first time the circuit opens for a given
	// pause, so that only one notification is sent.
	NotifyCircuit bool
	Until         time.Time
	RetryIn       time.Duration
	// Cooldown is true when the retry waits for the end of the pinned
	// account's rest in Magpie and the failure was not counted.
	Cooldown bool
}

// maxCooldownWait bounds the rest that defers a retry. A longer rest means
// the quota is still exhausted, which the normal retry and failure
// protection handle.
const maxCooldownWait = 10 * time.Minute

// Magpie answers a request pinned with X-Magpie-Account itself, before
// asking the vendor, when the account cannot take it (internal/gateway
// fallback.go, pinTo). Every such answer starts with the header's name:
//
//   - 429 "X-Magpie-Account: <account> rests until <RFC 3339> (<why>); no
//     other account is tried in its place": Magpie rests an account after
//     a rate limit, a used-up quota or a refusal until the vendor's reset.
//   - 429 "X-Magpie-Account: usage cap reached: ...", "spend cap
//     reached: ..." or "usage limit reached: every account ...": a cap the
//     user set on the account in Magpie, or an account Magpie holds as set
//     not to spend beyond its allowance (cappedError).
//   - 400 "<account>'s plan doesn't list <model>" and 404 "no account
//     <name> serves this model": the account does not serve the model.
const pinMarker = "x-magpie-account:"

var restUntilPattern = regexp.MustCompile(`rests until (\d{4}-\d{2}-\d{2}T[0-9:.]+(?:Z|[+-]\d{2}:\d{2}))`)

// pinHeldPattern matches Magpie's whole fixed wording of a held pinned
// account: the header's name, then either a rest with its RFC 3339 time or
// one of cappedError's openings right after it. A vendor's own "usage limit
// reached", such as Claude Code's "usage limit reached|<unix>", is a real
// limit and does not match.
var pinHeldPattern = regexp.MustCompile(`x-magpie-account: (?:[^\n]*? rests until \d{4}-\d{2}-\d{2}t|usage cap reached: |spend cap reached: |usage limit reached: every account )`)

// PinHeld reports whether err is Magpie holding the pinned account back
// without asking the vendor: a rest or a cap. It is no answer of the
// vendor, so it is no rate limit of the account.
func PinHeld(err error) bool {
	if err == nil {
		return false
	}
	return pinHeldPattern.MatchString(strings.ToLower(err.Error()))
}

// pinUnserved reports whether Magpie refused the model for the pinned
// account without asking the vendor.
func pinUnserved(text string) bool {
	return strings.Contains(text, pinMarker) &&
		(strings.Contains(text, "plan doesn't list") || strings.Contains(text, "serves this model"))
}

// CooldownWait returns how long Magpie still rests the pinned account,
// from the latest "rests until" time of the error, measured from now. An
// account that hit its limit rests until the vendor's reset, so an
// ignition at reset plus grace can arrive a few seconds too early.
func CooldownWait(err error, now time.Time) (time.Duration, bool) {
	if err == nil || !PinHeld(err) {
		return 0, false
	}
	var until time.Time
	for _, match := range restUntilPattern.FindAllStringSubmatch(err.Error(), -1) {
		if t, parseErr := time.Parse(time.RFC3339Nano, match[1]); parseErr == nil && t.After(until) {
			until = t
		}
	}
	wait := until.Sub(now)
	if until.IsZero() || wait <= 0 {
		return 0, false
	}
	return wait, wait <= maxCooldownWait
}

// RecordFailure updates the failure state. Hard errors and the last allowed
// transient error pause the target until the next daily start.
//
// A short rest of the pinned account in Magpie is waited out instead: the
// retry runs when the rest ends plus the grace period, and the failure is
// not counted. Only the first of consecutive rests gets this, so a rest
// that keeps renewing still reaches the failure protection.
func (s Schedule) RecordFailure(ts *store.TargetState, now time.Time, err error) FailureOutcome {
	if wait, ok := CooldownWait(err, now); ok && !s.recentCooldown(ts, now) {
		delay := wait + time.Duration(s.Cfg.GraceSeconds)*time.Second
		ts.LastError = truncate(errorText(err), 500)
		ts.LastFailureEpoch = epoch(now)
		ts.RetryAtEpoch = epoch(now.Add(delay))
		return FailureOutcome{RetryIn: delay, Cooldown: true}
	}
	ts.ConsecutiveFailures++
	ts.LastError = truncate(errorText(err), 500)
	ts.LastFailureEpoch = epoch(now)
	if Classify(err) == Hard || ts.ConsecutiveFailures >= s.Cfg.MaxTransientFailures {
		until := s.NextDailyStart(now)
		untilEpoch := float64(until.Unix())
		ts.CircuitOpenUntilEpoch = untilEpoch
		ts.RetryAtEpoch = untilEpoch
		ts.CircuitReason = truncate(errorText(err), 500)
		notify := ts.CircuitNotifiedUntilEpoch != untilEpoch
		ts.CircuitNotifiedUntilEpoch = untilEpoch
		return FailureOutcome{CircuitOpened: true, NotifyCircuit: notify, Until: until}
	}
	delay := time.Duration(s.Cfg.FailureRetrySeconds) * time.Second
	for i := 1; i < ts.ConsecutiveFailures; i++ {
		delay *= time.Duration(s.Cfg.FailureBackoffMultiplier)
	}
	ts.RetryAtEpoch = epoch(now.Add(delay))
	return FailureOutcome{RetryIn: delay}
}

// RecordCredentialFailure records a failure caused by the credential: the
// provider rejected it or disabled the account. Retrying cannot help, so no
// retry time is set; the engine skips the target until the credential
// changes or a manual ignition succeeds. summary is the error in one
// sentence, shown as the pause reason.
func RecordCredentialFailure(ts *store.TargetState, now time.Time, err error, summary string) {
	ts.ConsecutiveFailures++
	ts.LastError = truncate(errorText(err), 500)
	ts.LastFailureEpoch = epoch(now)
	ts.RetryAtEpoch = 0
	ts.CircuitOpenUntilEpoch = 0
	ts.CircuitReason = truncate(summary, 500)
}

// recentCooldown reports whether the previous failure, shortly before now,
// was already a rest that was waited out.
func (s Schedule) recentCooldown(ts *store.TargetState, now time.Time) bool {
	if ts.LastFailureEpoch == 0 || now.Sub(epochTime(ts.LastFailureEpoch)) > 2*maxCooldownWait {
		return false
	}
	return PinHeld(errors.New(ts.LastError))
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// Kind classifies an ignition error.
type Kind int

const (
	// Transient errors are retried with backoff.
	Transient Kind = iota
	// Hard errors pause the target until the next daily start.
	Hard
	// Unsupported means Magpie refused the model for the pinned account
	// before contacting the provider; the caller tries the next model.
	Unsupported
)

// ErrNotConfirmed means the request returned but no fixed 5-hour reset was
// observed afterwards.
var ErrNotConfirmed = errors.New("点火请求已返回，但未确认到新的固定 5h reset")

// ErrNoModel means no candidate model could be used for the account.
var ErrNoModel = errors.New("候选点火模型都不能用于该凭证")

var hardStatuses = map[int]bool{400: true, 401: true, 403: true, 404: true, 409: true, 422: true, 429: true}

// Classify sorts an ignition error into Transient, Hard or Unsupported.
func Classify(err error) Kind {
	if err == nil {
		return Transient
	}
	if errors.Is(err, ErrNotConfirmed) || errors.Is(err, ErrNoModel) {
		return Hard
	}
	text := strings.ToLower(err.Error())
	// Neither of these reached the provider.
	if pinUnserved(text) {
		return Unsupported
	}
	if PinHeld(err) {
		return Transient
	}
	if hardStatuses[host.StatusOf(err)] {
		return Hard
	}
	for _, marker := range []string{
		"http 400", "http 401", "http 403", "http 404", "http 409", "http 422", "http 429",
		"status 400", "status 401", "status 403", "status 404", "status 409", "status 422", "status 429",
		"not_found", "not found", "unsupported", "invalid model",
	} {
		if strings.Contains(text, marker) {
			return Hard
		}
	}
	return Transient
}

// ModelRejected reports whether the provider refused the model itself, with
// HTTP 400 or 404. Magpie lists a model for every account of a provider, so a
// listed model can still be refused by one account. Authentication errors and
// rate limits concern the account and are not model rejections.
func ModelRejected(err error) bool {
	if Classify(err) != Hard || errors.Is(err, ErrNotConfirmed) || errors.Is(err, ErrNoModel) {
		return false
	}
	// A disabled account refuses every model, sometimes with HTTP 400.
	if failure.Diagnose(err).Reason == failure.ReasonSuspended {
		return false
	}
	if status := host.StatusOf(err); status != 0 {
		return status == 400 || status == 404
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"http 400", "http 404", "status 400", "status 404", "not_found", "not found", "invalid model"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
