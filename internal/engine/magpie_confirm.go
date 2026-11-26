package engine

import (
	"context"
	"errors"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
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
// Magpie's kept readings, read every keptInterval, can confirm the window
// earlier (runMagpieConfirms).
var magpieConfirmDelays = []time.Duration{5 * time.Second, time.Minute, time.Minute}

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

// confirmedByMagpie reports whether w, the 5-hour window of a reading
// Magpie made after the ignition request was sent (base.start), shows a
// running window at now. Magpie stamps its readings when agy has
// finished, some seconds after Google read them, so the reading time
// cannot tell a rolling placeholder from a window that started; the reset
// can. A request sent at start and answered at end starts a window that
// resets between start and end plus 5 hours. A placeholder Google gives
// later moves with each reading and resets after end plus 5 hours: the
// first confirming refresh starts magpieConfirmDelays[0] after end, and
// agy takes seconds more to read. A reset before start plus 5 hours is
// of a window that started before the request, which runs as well: an
// ended window's reset has passed, and a placeholder read before the
// request never comes back unchanged, since Google moves it with every
// read.
func confirmedByMagpie(w WindowView, base ignitionBase, now time.Time) bool {
	if w.ReadAt.Before(base.start) || w.Reset.IsZero() || !w.Reset.After(now) {
		return false
	}
	end := base.end
	if end.Before(base.start) {
		end = base.start
	}
	return !w.Reset.After(end.Add(5*time.Hour + magpieResetTolerance))
}

// confirmMagpie starts the confirmation of the ignition of t, an account
// read only from Magpie, and returns ErrIgnitionPending; it returns nil
// when a reading already confirms the window.
func (e *Engine) confirmMagpie(t target, base ignitionBase, model, fingerprint string, manual bool) error {
	if e.confirmed(t.group.Key, base, e.now()) {
		return nil
	}
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

// confirmStep handles the end of a refresh that confirms the ignition of
// the target key; err is the refresh's error. It records the outcome when
// the window is confirmed or when this was the last refresh, and otherwise
// schedules the next one.
func (e *Engine) confirmStep(ctx context.Context, cfg config.Config, key string, err error) {
	c := e.magpieConfirms[key]
	if c == nil {
		return
	}
	c.running = false
	c.done++
	now := e.now()
	if e.confirmed(key, c.base, now) {
		e.finishMagpieConfirm(ctx, cfg, key, nil)
		return
	}
	if err != nil && ctx.Err() == nil {
		e.logf("warn", "点火确认时刷新额度失败：%s：%v", c.target.group.Label, err)
	}
	if c.done < len(magpieConfirmDelays) {
		c.next = now.Add(magpieConfirmDelays[c.done])
		return
	}
	e.finishMagpieConfirm(ctx, cfg, key, ignite.ErrNotConfirmed)
}

// runMagpieConfirms records at once the ignitions whose window Magpie's
// readings have confirmed meanwhile.
func (e *Engine) runMagpieConfirms(ctx context.Context, cfg config.Config) {
	for key, c := range e.magpieConfirms {
		if ctx.Err() != nil || e.stopping() {
			return
		}
		if e.confirmed(key, c.base, e.now()) {
			e.finishMagpieConfirm(ctx, cfg, key, nil)
		}
	}
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
