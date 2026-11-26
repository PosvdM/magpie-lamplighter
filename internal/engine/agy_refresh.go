package engine

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// Every refresh of an account read only from Magpie (Antigravity) has
// Magpie run agy's /usage, which takes from 5 seconds to more than 30.
// The loop never waits for one: it queues the refreshes, and a background
// worker (agyWorker) runs one at a time and hands each result back to the
// loop, which applies it (agyFinished). The worker takes, in this order:
//
//  1. the refreshes that confirm an ignition, when due (magpieConfirms);
//  2. the manual refreshes (queueAgyManual), in the order asked;
//  3. the scheduled ones: of the accounts a scheduled round found stale
//     (queryMagpie), the one whose last refresh of any kind was tried
//     longest ago, at least magpieRefreshGap after it.
//
// Each refresh runs within scheduledRenewTimeout. The time of an
// account's last refresh (renewedAt) is when the host sent it, which
// package magpie counts its own refresh gap from as well; a refresh the
// host refused for that gap counts as none. Only the refreshes of accounts
// still loaded whose service is monitored run: the queues drop the others
// (pruneAgy), and the worker checks again right before the host sends a
// refresh. A refresh the stop of the loop cut short counts as none
// either: it changes neither the account's state nor a confirmation.

// agyJobKind is why the worker refreshes an account.
type agyJobKind int

const (
	agyScheduled agyJobKind = iota
	agyManual
	agyConfirm
)

// agyJob is one refresh for the worker: the account pin, and before, the
// time of the account's reading applied when the job was taken. key is
// the quota group a confirmation refresh is for. tried is the account's
// renewedAt before the job, restored when the job counts as no refresh.
type agyJob struct {
	pin    string
	kind   agyJobKind
	key    string
	before time.Time
	tried  time.Time
}

// agyDone is the result of a job, handed back to the loop.
type agyDone struct {
	job    agyJob
	result refreshResult
}

// startAgyWorker starts the worker of this run of the loop; it ends with
// ctx. The engine's queues outlive a restart of the loop, but not a job
// in flight: a new run runs it again.
func (e *Engine) startAgyWorker(ctx context.Context) {
	jobs, done := make(chan agyJob, 1), make(chan agyDone, 1)
	e.agyJobs, e.agyDoneCh, e.agyBusy = jobs, done, nil
	for _, c := range e.magpieConfirms {
		c.running = false
	}
	for pin, waiting := range e.agyWaiters {
		if len(waiting) > 0 && !containsString(e.agyManualQ, pin) {
			e.agyManualQ = append(e.agyManualQ, pin)
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-jobs:
				result := e.runAgyJob(ctx, job)
				if ctx.Err() != nil {
					// The run of the loop ended: its result is not
					// applied, and the next run runs the job again.
					return
				}
				select {
				case done <- agyDone{job: job, result: result}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

// runAgyJob runs the refresh of job. It changes no state of the engine;
// right before the host sends the refresh, it checks that the account may
// still be refreshed (agyRefreshable). A refresh that confirms an
// ignition is not held by the account's gap in package magpie
// (host.ConfirmingIgnition).
func (e *Engine) runAgyJob(ctx context.Context, job agyJob) refreshResult {
	renew := ctx
	if job.kind == agyConfirm {
		renew = host.ConfirmingIgnition(ctx)
	}
	renew, cancel := context.WithTimeout(renew, scheduledRenewTimeout)
	defer cancel()
	return e.fetchRefresh(ctx, renew, job.pin, func() error { return e.agyRefreshable(job.pin) })
}

// agyRefreshable returns errRefreshAccountGone when the account pin may no
// longer be refreshed: it is no longer loaded, or the settings as they
// are now no longer monitor its service. Changing the settings does not
// reload the accounts. Safe for the worker.
func (e *Engine) agyRefreshable(pin string) error {
	cfg := e.config()
	e.mu.Lock()
	cred := e.creds[pin]
	e.mu.Unlock()
	if cred == nil || !cfg.Provider(cred.Provider).Monitor {
		return errRefreshAccountGone
	}
	return nil
}

// pruneAgy drops the refreshes of accounts that may no longer be
// refreshed (agyRefreshable): a confirmation ends as withdrawn, not as a
// failure, and the manual refreshes waiting for the account get
// errRefreshAccountGone.
func (e *Engine) pruneAgy(ctx context.Context, cfg config.Config) {
	pins := map[string]bool{}
	for _, c := range e.magpieConfirms {
		if !c.running {
			pins[c.target.cred.AuthIndex] = true
		}
	}
	for _, pin := range e.agyManualQ {
		pins[pin] = true
	}
	for pin := range e.agyStale {
		pins[pin] = true
	}
	for pin := range pins {
		if (e.agyBusy == nil || e.agyBusy.pin != pin) && e.agyRefreshable(pin) != nil {
			e.dropAgy(ctx, cfg, pin)
		}
	}
}

// dropAgy drops every refresh of the account pin; see pruneAgy.
func (e *Engine) dropAgy(ctx context.Context, cfg config.Config, pin string) {
	for key, c := range e.magpieConfirms {
		if c.target.cred.AuthIndex == pin {
			e.finishMagpieConfirm(ctx, cfg, key, errIgnitionWithdrawn)
		}
	}
	for _, ch := range e.agyWaiters[pin] {
		ch <- errRefreshAccountGone
	}
	delete(e.agyWaiters, pin)
	e.agyManualQ = slices.DeleteFunc(e.agyManualQ, func(p string) bool { return p == pin })
	delete(e.agyStale, pin)
}

// queueAgyManual queues a manual refresh of the account pin and returns
// the channel that gets its error once the loop has applied it. A refresh
// of the account already running answers it too.
func (e *Engine) queueAgyManual(pin string) <-chan error {
	ch := make(chan error, 1)
	e.agyWaiters[pin] = append(e.agyWaiters[pin], ch)
	if (e.agyBusy == nil || e.agyBusy.pin != pin) && !containsString(e.agyManualQ, pin) {
		e.agyManualQ = append(e.agyManualQ, pin)
	}
	return ch
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// agyStaleDue reports whether the account pin, marked stale by a scheduled
// round, still needs a refresh at now, and when it may have one; a
// refresh is due when due is not after now. An account no longer loaded,
// or with a fresh reading meanwhile, needs none.
func (e *Engine) agyStaleDue(pin string, now time.Time) (due time.Time, needed bool) {
	e.mu.Lock()
	loaded := e.creds[pin] != nil
	e.mu.Unlock()
	if !loaded {
		return time.Time{}, false
	}
	if last := e.keptAt[pin]; !last.IsZero() && now.Sub(last) < magpieStaleAfter {
		return time.Time{}, false
	}
	if last, ok := e.renewedAt[pin]; ok {
		return last.Add(magpieRefreshGap), true
	}
	return time.Time{}, true
}

// nextAgyJob takes the job the worker runs next at now, by the order
// above, and marks it as running; ok is false when the worker is busy or
// nothing is due.
func (e *Engine) nextAgyJob(now time.Time) (job agyJob, ok bool) {
	if e.agyBusy != nil {
		return agyJob{}, false
	}
	var confirm *magpieConfirm
	for _, c := range e.magpieConfirms {
		if !c.running && !c.next.After(now) && (confirm == nil || c.next.Before(confirm.next)) {
			confirm = c
		}
	}
	switch {
	case confirm != nil:
		confirm.running = true
		job = agyJob{pin: confirm.target.cred.AuthIndex, kind: agyConfirm, key: confirm.target.group.Key}
	default:
		for len(e.agyManualQ) > 0 && !ok {
			pin := e.agyManualQ[0]
			e.agyManualQ = e.agyManualQ[1:]
			if len(e.agyWaiters[pin]) > 0 {
				job, ok = agyJob{pin: pin, kind: agyManual}, true
			}
		}
		if !ok {
			var pins []string
			for pin := range e.agyStale {
				due, needed := e.agyStaleDue(pin, now)
				switch {
				case !needed:
					delete(e.agyStale, pin)
				case !due.After(now):
					pins = append(pins, pin)
				}
			}
			if len(pins) == 0 {
				return agyJob{}, false
			}
			// The account whose last refresh was tried longest ago, one
			// never tried first.
			sort.Slice(pins, func(i, j int) bool {
				a, b := e.renewedAt[pins[i]], e.renewedAt[pins[j]]
				if !a.Equal(b) {
					return a.Before(b)
				}
				return pins[i] < pins[j]
			})
			job = agyJob{pin: pins[0], kind: agyScheduled}
			delete(e.agyStale, pins[0])
		}
	}
	job.before = e.keptAt[job.pin]
	job.tried = e.renewedAt[job.pin]
	e.renewedAt[job.pin] = now
	e.agyBusy = &job
	return job, true
}

// dispatchAgy drops the refreshes that may no longer run (pruneAgy) and
// hands the next job to the worker of this run, if any is due and the
// worker is free.
func (e *Engine) dispatchAgy(ctx context.Context) {
	if e.agyJobs == nil {
		return
	}
	e.pruneAgy(ctx, e.config())
	job, ok := e.nextAgyJob(e.now())
	if !ok {
		return
	}
	select {
	case e.agyJobs <- job:
	default:
		// Not reached: the worker takes a job only while agyBusy is nil.
		e.agyBusy = nil
	}
}

// nextAgyWake is when a job of the worker comes due, zero for none or
// while the worker is busy: its result wakes the loop.
func (e *Engine) nextAgyWake(now time.Time) time.Time {
	if e.agyBusy != nil {
		return time.Time{}
	}
	var next time.Time
	consider := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, c := range e.magpieConfirms {
		if !c.running {
			consider(c.next)
		}
	}
	if len(e.agyManualQ) > 0 {
		consider(now)
	}
	for pin := range e.agyStale {
		if due, needed := e.agyStaleDue(pin, now); needed {
			consider(due)
		}
	}
	return next
}

// agyFinished applies the result of a job in the loop: the time the
// refresh was sent, the reading it brought, the account's error in the
// status, the manual refreshes waiting for the account, and the
// confirmations the reading settles.
//
// A job the stop of the loop cut short counts as no refresh: its
// confirmation is no try, nothing is recorded, and the waiting manual
// refreshes are answered by the stop. A job the worker stopped because the
// account may no longer be refreshed drops the account's refreshes
// (dropAgy). A refresh the host refused for the account's gap counts as
// none: a scheduled one is due again when the gap ends, and is not shown
// as the account's error unless a manual refresh waits for it.
func (e *Engine) agyFinished(ctx context.Context, cfg config.Config, d agyDone) {
	if e.agyBusy != nil && e.agyBusy.pin == d.job.pin {
		e.agyBusy = nil
	}
	pin, r := d.job.pin, d.result
	untried := func() {
		if d.job.tried.IsZero() {
			delete(e.renewedAt, pin)
		} else {
			e.renewedAt[pin] = d.job.tried
		}
	}
	if r.err != nil && (ctx.Err() != nil || e.stopping()) {
		untried()
		if c := e.magpieConfirms[d.job.key]; d.job.kind == agyConfirm && c != nil {
			c.running = false
		}
		return
	}
	limited := r.sentAt.IsZero() && errors.Is(r.err, host.ErrRefreshLimited)
	switch {
	case !r.sentAt.IsZero():
		e.renewedAt[pin] = r.sentAt
	case limited:
		untried()
		// At least agyLimitedRetry before the next try, should the host
		// count from a later time.
		if floor := e.now().Add(agyLimitedRetry - magpieRefreshGap); e.renewedAt[pin].Before(floor) {
			e.renewedAt[pin] = floor
		}
	case errors.Is(r.err, errRefreshAccountGone):
		untried()
		if c := e.magpieConfirms[d.job.key]; d.job.kind == agyConfirm && c != nil {
			c.running = false
		}
		e.dropAgy(ctx, cfg, pin)
		e.dirty = true
		return
	}
	if limited && d.job.kind == agyScheduled {
		e.agyStale[pin] = true
		if len(e.agyWaiters[pin]) == 0 {
			return
		}
	}
	e.mu.Lock()
	cred := e.creds[pin]
	e.mu.Unlock()
	var err error
	if cred == nil {
		err = errRefreshAccountGone
	} else {
		err = e.settleRefresh(ctx, cred, d.job.before, r)
		if err != nil {
			e.agyErrs[pin] = err
			if d.job.kind != agyConfirm {
				e.logf("warn", "额度刷新失败：%s：%v", accountLabel(cred), err)
			}
		} else {
			delete(e.agyErrs, pin)
		}
		e.mu.Lock()
		if err != nil {
			e.pollErrs[pin] = err.Error()
		} else {
			delete(e.pollErrs, pin)
		}
		e.mu.Unlock()
	}
	for _, ch := range e.agyWaiters[pin] {
		ch <- err
	}
	delete(e.agyWaiters, pin)
	e.dirty = true
	e.confirmByRefresh(ctx, cfg, d)
	if d.job.kind == agyConfirm {
		e.confirmStep(ctx, cfg, d.job.key, err)
	}
}

// agyLimitedRetry is the least time before a scheduled refresh the host
// refused for the account's gap is tried again.
const agyLimitedRetry = time.Minute

// ErrRefreshQueued is the answer to a manual refresh whose refresh by
// Magpie had not finished by the end of the manual refresh: it is queued
// or running, and its result shows in the status when it comes.
var ErrRefreshQueued = errors.New("刷新已排队，Magpie 读到后结果会显示在状态中")

// awaitManual waits for the refreshes of a manual refresh (e.manualWaits)
// until end, and returns err, the error of the manual round, else the
// first error of the refreshes for a refresh of one account, else
// ErrRefreshQueued when one did not finish in time. Run by the goroutine
// that replies to the command, not by the loop.
func (e *Engine) awaitManual(err error, one bool, waits []<-chan error, end time.Time) error {
	timer := time.NewTimer(time.Until(end))
	defer timer.Stop()
	for _, ch := range waits {
		select {
		case refreshErr := <-ch:
			if err == nil && one {
				err = refreshErr
			}
		case <-timer.C:
			if err == nil {
				err = ErrRefreshQueued
			}
			return err
		case <-e.stopCh:
			return ErrStopped
		}
	}
	return err
}
