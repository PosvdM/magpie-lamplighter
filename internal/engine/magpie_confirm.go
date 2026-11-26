package engine

import (
	"context"
	"errors"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
)

// The ignition of an account whose quota is read only from Magpie
// (Antigravity) is confirmed by Magpie refreshing the account. Each refresh
// runs agy's /usage, and the agy-cli plugin waits at most 17 seconds for it:
// a slower read answers "still reading" and leaves its result to a refresh
// within the next 3 minutes. An ignition therefore asks for at most
// len(magpieConfirmDelays) refreshes: the first magpieConfirmDelays[0] after
// the request, in the ignition itself, and the others at least
// magpieConfirmDelays[i] apart, run by the background loop
// (runMagpieConfirms) so that they do not hold back the other targets. These
// refreshes are not held by the account's refresh gap
// (host.ConfirmingIgnition). Magpie's kept readings, read every keptInterval,
// can confirm the window earlier.
var magpieConfirmDelays = []time.Duration{5 * time.Second, time.Minute, time.Minute}

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
	// next is when the next refresh is due, and done how many refreshes
	// were asked for.
	next time.Time
	done int
}

// confirmPending reports whether the ignition of the target key is still
// being confirmed.
func (e *Engine) confirmPending(key string) bool {
	_, ok := e.magpieConfirms[key]
	return ok
}

// confirmMagpie confirms the ignition of t, an account read only from
// Magpie, with the first refresh; when that does not confirm the window, the
// loop takes over (runMagpieConfirms) and ErrIgnitionPending is returned. A
// stop or an ended ctx returns context.Canceled.
func (e *Engine) confirmMagpie(ctx context.Context, cfg config.Config, t target, base ignitionBase, model, fingerprint string, manual bool) error {
	if e.confirmed(t.group.Key, base, e.now()) {
		return nil
	}
	if !e.waitWithUsage(ctx, magpieConfirmDelays[0]) {
		return context.Canceled
	}
	if e.confirmMagpieOnce(ctx, cfg, t, base) {
		return nil
	}
	if ctx.Err() != nil || e.stopping() {
		return context.Canceled
	}
	if len(magpieConfirmDelays) < 2 {
		return ignite.ErrNotConfirmed
	}
	if e.magpieConfirms == nil {
		e.magpieConfirms = map[string]*magpieConfirm{}
	}
	e.magpieConfirms[t.group.Key] = &magpieConfirm{
		target: t, base: base, model: model, fingerprint: fingerprint, manual: manual,
		next: e.now().Add(magpieConfirmDelays[1]), done: 1,
	}
	e.logf("info", "%s：点火请求已发出，还没有读到新的 5 小时重置，稍后再请 Magpie 读取", t.group.Label)
	return ErrIgnitionPending
}

// confirmMagpieOnce has Magpie refresh the account of t once, outside its
// refresh gap, and reports whether the window is confirmed.
func (e *Engine) confirmMagpieOnce(ctx context.Context, cfg config.Config, t target, base ignitionBase) bool {
	if e.confirmed(t.group.Key, base, e.now()) {
		return true
	}
	refresh, cancel := context.WithTimeout(host.ConfirmingIgnition(ctx), scheduledRenewTimeout)
	defer cancel()
	e.renewedAt[t.cred.AuthIndex] = e.now()
	err := e.refreshKept(ctx, refresh, cfg, t.cred)
	if e.confirmed(t.group.Key, base, e.now()) {
		return true
	}
	if err != nil && ctx.Err() == nil {
		e.logf("warn", "点火确认时刷新额度失败：%s：%v", t.group.Label, err)
	}
	return false
}

// runMagpieConfirms continues the confirmations left to the loop: a window
// that Magpie's readings confirmed meanwhile is recorded at once; a due one
// has Magpie refresh the account again. After the last refresh the ignition
// is recorded as not confirmed.
func (e *Engine) runMagpieConfirms(ctx context.Context, cfg config.Config) {
	for key, c := range e.magpieConfirms {
		if ctx.Err() != nil || e.stopping() {
			return
		}
		now := e.now()
		confirmed := e.confirmed(key, c.base, now)
		if !confirmed && now.Before(c.next) {
			continue
		}
		if !confirmed {
			confirmed = e.confirmMagpieOnce(ctx, cfg, c.target, c.base)
			if ctx.Err() != nil || e.stopping() {
				return
			}
			c.done++
		}
		var err error
		if !confirmed {
			if c.done < len(magpieConfirmDelays) {
				c.next = e.now().Add(magpieConfirmDelays[c.done])
				continue
			}
			err = ignite.ErrNotConfirmed
		}
		delete(e.magpieConfirms, key)
		e.finishIgnition(ctx, cfg, c.target, c.manual, c.model, c.fingerprint, err)
	}
}

// nextMagpieConfirm is when the next refresh of runMagpieConfirms is due,
// zero for none.
func (e *Engine) nextMagpieConfirm() time.Time {
	var next time.Time
	for _, c := range e.magpieConfirms {
		if next.IsZero() || c.next.Before(next) {
			next = c.next
		}
	}
	return next
}
