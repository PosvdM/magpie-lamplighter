package engine

import (
	"context"
	"errors"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// The ignition of an account whose quota is read only from Magpie
// (Antigravity) is confirmed by Magpie refreshing the account. Each refresh
// runs agy's /usage, and the agy-cli plugin waits at most 17 seconds for it:
// a slower read answers "still reading" and leaves its result to a refresh
// within the next 3 minutes. An ignition therefore asks for at most
// len(magpieConfirmDelays) refreshes, the first magpieConfirmDelays[0]
// after the request was answered and each other magpieConfirmDelays[i]
// after the previous one ended. The background worker runs them
// (agy_refresh.go), so the ignition returns ErrIgnitionPending at once and
// the loop records the outcome later (confirmStep). These refreshes are
// not held by the account's refresh gap (host.ConfirmingIgnition).
//
// Only the card a refresh answered can confirm the window, of a refresh
// Lamplighter's worker sent magpieConfirmAfter or more after the request
// was answered: confirming refreshes, and manual or scheduled ones, which
// can confirm it earlier (confirmByRefresh). Magpie's kept quota, and a
// refresh sent earlier, can hold a reading agy began before the request:
// Magpie stamps readAt only when agy has finished. Magpie's refresh can
// still answer such a reading: it reads a card at most once in 30 seconds,
// and both Magpie and the agy-cli plugin join a read of the account under
// way. So the window must also show quota used, which the ignition uses
// and a placeholder has none of; see confirmedByMagpie.
var magpieConfirmDelays = []time.Duration{magpieConfirmAfter, time.Minute, time.Minute}

// magpieConfirmAfter is how long after the ignition request was answered
// a refresh is sent at the earliest to confirm it. agy takes seconds more
// to read, so the placeholder of a window that has not started resets
// more than magpieResetTolerance after the answer plus 5 hours.
const magpieConfirmAfter = 5 * time.Second

// magpieResetTolerance is the leeway of a confirming reset around the
// times the ignition request was sent and answered: Google gives resets to
// the second, and the clocks of this machine and Google's differ a little.
const magpieResetTolerance = 3 * time.Second

// ErrIgnitionPending is returned by igniteTarget, and by Ignite, when the
// window of an ignition is still to be confirmed by the background loop,
// which records the outcome as an event.
var ErrIgnitionPending = errors.New("点火请求已发出，正在等待 Magpie 读到新的 5 小时重置，结果稍后记入事件")

// errConfirmBusy refuses an ignition of a target whose last ignition is
// still being confirmed.
var errConfirmBusy = errors.New("上一次点火仍在等待确认")

// magpieConfirm is an ignition whose window the loop still confirms.
type magpieConfirm struct {
	target      target
	base        ignitionBase
	model       string
	fingerprint string
	manual      bool
	// next is when the next refresh is due, done how many refreshes have
	// ended, and running whether the worker runs one.
	next    time.Time
	done    int
	running bool
}

// confirmPending reports whether the ignition of the target key is still
// being confirmed.
func (e *Engine) confirmPending(key string) bool {
	_, ok := e.magpieConfirms[key]
	return ok
}

// confirmedByMagpie reports whether w, the 5-hour window of a card read at
// readAt that a refresh sent magpieConfirmAfter or more after the
// ignition request was answered brought, shows a running window at now. A
// request sent at base.start and answered at base.end starts a window that
// resets between start and end plus 5 hours, and uses some of its quota.
// A placeholder has nothing used, and one Google gives after the refresh
// was sent resets after end plus 5 hours plus magpieResetTolerance. A
// reset before start plus 5 hours, with quota used, is of a window that
// started before the request, which runs as well.
func confirmedByMagpie(w quota.Window, readAt time.Time, base ignitionBase, now time.Time) bool {
	if readAt.Before(base.start) || w.Remaining >= 100 || w.Reset.IsZero() || !w.Reset.After(now) {
		return false
	}
	end := base.end
	if end.Before(base.start) {
		end = base.start
	}
	return !w.Reset.After(end.Add(5*time.Hour + magpieResetTolerance))
}

// confirmMagpie starts the confirmation of the ignition of t, an account
// read only from Magpie, and returns ErrIgnitionPending.
func (e *Engine) confirmMagpie(t target, base ignitionBase, model, fingerprint string, manual bool) error {
	if len(magpieConfirmDelays) == 0 {
		return ignite.ErrNotConfirmed
	}
	e.magpieConfirms[t.group.Key] = &magpieConfirm{
		target: t, base: base, model: model, fingerprint: fingerprint, manual: manual,
		next: e.now().Add(magpieConfirmDelays[0]),
	}
	e.logf("info", "%s：点火请求已发出，稍后请 Magpie 读取额度确认", t.group.Label)
	return ErrIgnitionPending
}

// confirmByRefresh records the ignitions of the account of d whose window
// the card its refresh answered confirms (confirmedByMagpie): a refresh
// that succeeded, sent magpieConfirmAfter or more after the ignition
// request was answered.
func (e *Engine) confirmByRefresh(ctx context.Context, cfg config.Config, d agyDone) {
	r := d.result
	if r.err != nil || !r.answered || r.sentAt.IsZero() {
		return
	}
	now := e.now()
	for key, c := range e.magpieConfirms {
		if c.target.cred.AuthIndex != d.job.pin || r.sentAt.Before(c.base.end.Add(magpieConfirmAfter)) {
			continue
		}
		if w, ok := cardFiveHour(r.card, c.target.cred, key); ok && confirmedByMagpie(w, r.card.ReadAt, c.base, now) {
			e.finishMagpieConfirm(ctx, cfg, key, nil)
		}
	}
}

// cardFiveHour is the 5-hour window of the group key in k, a reading of
// cred.
func cardFiveHour(k host.KeptQuota, cred *Cred, key string) (quota.Window, bool) {
	for _, g := range k.Groups {
		if cred.Provider+":"+cred.AuthIndex+":"+g.Key != key {
			continue
		}
		for _, w := range g.Windows {
			if quota.IsFiveHour(w) {
				return w, true
			}
		}
	}
	return quota.Window{}, false
}

// confirmStep counts the end of a refresh that confirms the ignition of
// the target key, unless its card confirmed it (confirmByRefresh); err is
// the refresh's error. It records the failure when this was the last
// refresh, and otherwise schedules the next one.
func (e *Engine) confirmStep(ctx context.Context, cfg config.Config, key string, err error) {
	c := e.magpieConfirms[key]
	if c == nil {
		return
	}
	c.running = false
	c.done++
	if err != nil {
		e.logf("warn", "点火确认时刷新额度失败：%s：%v", c.target.group.Label, err)
	}
	if c.done < len(magpieConfirmDelays) {
		c.next = e.now().Add(magpieConfirmDelays[c.done])
		return
	}
	e.finishMagpieConfirm(ctx, cfg, key, ignite.ErrNotConfirmed)
}

// finishMagpieConfirm records the outcome of the ignition of the target
// key. The confirming reset is fixed: the ignition started the window, or
// it ran already, whatever the reading before it showed.
func (e *Engine) finishMagpieConfirm(ctx context.Context, cfg config.Config, key string, err error) {
	c := e.magpieConfirms[key]
	if c == nil {
		return
	}
	delete(e.magpieConfirms, key)
	if err == nil {
		e.state.Target(key).RollingReset = false
		e.mu.Lock()
		if live := e.groups[key]; live != nil {
			live.rolling = false
		}
		e.mu.Unlock()
	}
	e.finishIgnition(ctx, cfg, c.target, c.manual, c.model, c.fingerprint, err)
	e.refreshTargets(cfg)
}
