package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/notify"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// maxCandidates limits how many models one ignition tries.
const maxCandidates = 6

// exhaustedWindow returns a window of the group other than the 5-hour one
// that is used up and has not reset yet. While it lasts, every request on
// the account is rejected, so ignition waits for it.
func exhaustedWindow(g GroupView, now time.Time) (WindowView, bool) {
	for _, w := range g.Windows {
		if quota.IsFiveHour(w.Window) {
			continue
		}
		if w.Remaining <= notify.ExhaustedRemaining && (w.Reset.IsZero() || w.Reset.After(now)) {
			return w, true
		}
	}
	return WindowView{}, false
}

// TargetView is the ignition status of one quota group.
type TargetView struct {
	Key                 string    `json:"key"`
	Label               string    `json:"label"`
	Provider            string    `json:"provider"`
	AuthIndex           string    `json:"auth_index"`
	NextDue             time.Time `json:"next_due"`
	Reset               time.Time `json:"reset,omitempty"`
	Rolling             bool      `json:"rolling"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastAttempt         time.Time `json:"last_attempt,omitempty"`
	LastModel           string    `json:"last_model,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CircuitUntil        time.Time `json:"circuit_until,omitempty"`
	CircuitReason       string    `json:"circuit_reason,omitempty"`
	// BlockedWindow names a used-up window that holds ignition back, and
	// BlockedUntil is its reset, zero when unknown.
	BlockedWindow string    `json:"blocked_window,omitempty"`
	BlockedUntil  time.Time `json:"blocked_until,omitempty"`
}

type target struct {
	group  GroupView
	cred   *Cred
	reset  time.Time
	source string
	// automatic marks the requests of an automatic ignition, which pass
	// admit before they are sent.
	automatic bool
}

// groupIgnitable reports whether ignition is enabled for the group: its
// provider is monitored and ignited, and the group is selected. For a
// provider with group checkboxes (providers.Ignition.Groups), groups lists
// the checked ones: an empty list selects none, and a list that is absent
// or null selects the default groups. For other providers an empty list
// selects every group.
func groupIgnitable(cfg config.Config, provider, sourceLabel string) bool {
	spec := providers.Lookup(provider)
	if spec == nil || spec.Ignition == nil {
		return false
	}
	p := cfg.Provider(provider)
	if !p.Monitor || !p.Ignite {
		return false
	}
	groups := p.Groups
	if len(spec.Ignition.Groups) > 0 {
		if groups == nil {
			groups = spec.Ignition.DefaultGroups
		}
		if len(groups) == 0 {
			return false
		}
	} else if len(groups) == 0 {
		return true
	}
	label := strings.ToLower(strings.TrimSpace(sourceLabel))
	for _, allowed := range groups {
		if strings.ToLower(strings.TrimSpace(allowed)) == label {
			return true
		}
	}
	return false
}

// ignitionTarget reports whether group is an ignition target under cfg: it
// has a 5-hour window and groupIgnitable accepts it. ignition.enabled is
// checked by the callers: a manual ignition ignores it. Automatic and
// manual ignition, the admission of each automatic request (admit) and the
// magpie_warmup list of the status all use this rule.
func ignitionTarget(cfg config.Config, group *GroupView) bool {
	if !groupIgnitable(cfg, group.Provider, group.SourceLabel) {
		return false
	}
	_, ok := group.fiveHour()
	return ok
}

// collectTargets returns the groups that have a 5-hour window and ignition
// enabled.
func (e *Engine) collectTargets(cfg config.Config) []target {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []target
	for _, group := range e.groups {
		cred := e.creds[group.AuthIndex]
		if cred == nil || !ignitionTarget(cfg, group) {
			continue
		}
		w, _ := group.fiveHour()
		snapshot := *group
		snapshot.Windows = append([]WindowView(nil), group.Windows...)
		out = append(out, target{group: snapshot, cred: cred, reset: w.Reset, source: string(w.Source)})
	}
	sortTargets(out)
	return out
}

func sortTargets(targets []target) {
	for i := 1; i < len(targets); i++ {
		for j := i; j > 0 && targets[j].group.Label < targets[j-1].group.Label; j-- {
			targets[j], targets[j-1] = targets[j-1], targets[j]
		}
	}
}

func (e *Engine) nextDue(cfg config.Config) time.Time {
	if e.state == nil {
		return time.Time{}
	}
	schedule := e.schedule(cfg)
	now := e.now()
	var next time.Time
	for _, t := range e.collectTargets(cfg) {
		if e.credentialBlocked(t.cred.AuthIndex) {
			continue
		}
		due := schedule.DueAt(e.state.Target(t.group.Key), t.reset, now)
		if w, blocked := exhaustedWindow(t.group, now); blocked {
			// Wake at the reset; the next poll shows whether the quota is back.
			if w.Reset.IsZero() {
				continue
			}
			due = w.Reset
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}

// refreshTargets rebuilds the ignition status shown on the page.
func (e *Engine) refreshTargets(cfg config.Config) {
	if e.state == nil {
		return
	}
	schedule := e.schedule(cfg)
	now := e.now()
	targets := e.collectTargets(cfg)
	views := make([]TargetView, 0, len(targets))
	for _, t := range targets {
		ts := e.state.Target(t.group.Key)
		view := TargetView{
			Key:                 t.group.Key,
			Label:               t.group.Label,
			Provider:            t.group.Provider,
			AuthIndex:           t.group.AuthIndex,
			Reset:               t.reset,
			Rolling:             ts.RollingReset,
			LastSuccess:         epochTime(ts.LastSuccessEpoch),
			LastAttempt:         epochTime(ts.LastAttemptEpoch),
			LastModel:           ts.LastModel,
			LastError:           ts.LastError,
			ConsecutiveFailures: ts.ConsecutiveFailures,
			CircuitReason:       ts.CircuitReason,
		}
		if until := epochTime(ts.CircuitOpenUntilEpoch); until.After(now) {
			view.CircuitUntil = until
		}
		if w, blocked := exhaustedWindow(t.group, now); blocked {
			view.BlockedWindow = quota.ShortLabel(w.Label)
			view.BlockedUntil = w.Reset
		} else if cfg.Ignition.Enabled && !e.credentialBlocked(t.cred.AuthIndex) {
			view.NextDue = schedule.DueAt(ts, t.reset, now)
		}
		views = append(views, view)
	}
	e.mu.Lock()
	e.targets = views
	e.mu.Unlock()
}

func epochTime(epoch float64) time.Time {
	if epoch <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(epoch*1e9)).UTC()
}

// performDue ignites every target whose due time has come.
func (e *Engine) performDue(ctx context.Context, cfg config.Config) bool {
	schedule := e.schedule(cfg)
	attempted := false
	for _, t := range e.collectTargets(cfg) {
		if ctx.Err() != nil {
			break
		}
		now := e.now()
		// A used-up window, such as the 7-day quota, rejects every request
		// until it resets; scheduled ignition skips the target until then.
		if _, blocked := exhaustedWindow(t.group, now); blocked {
			continue
		}
		// A rejected credential or a disabled account waits for a new
		// credential; see credentialBlocked.
		if e.credentialBlocked(t.cred.AuthIndex) {
			continue
		}
		due := schedule.DueAt(e.state.Target(t.group.Key), t.reset, now)
		if due.After(now.Add(250 * time.Millisecond)) {
			continue
		}
		if !e.mayIgnite(t) {
			continue
		}
		e.igniteTarget(ctx, cfg, t, false)
		attempted = true
	}
	return attempted
}

// errIgnitionWithdrawn ends an automatic ignition whose target stopped
// qualifying before its next request; see mayIgnite.
var errIgnitionWithdrawn = errors.New("点火条件已不满足，未发送的请求已放弃")

// mayIgnite reports whether an automatic ignition of t may start. See
// mayIgniteLocked.
func (e *Engine) mayIgnite(t target) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mayIgniteLocked(t)
}

// mayIgniteLocked reports whether an automatic ignition of t may send its
// next request under the current settings. The caller holds mu. The
// settings are read again for every request: a round runs for minutes with
// candidate models and confirmations, and the user can turn ignition off
// meanwhile. The target must still qualify: ignition.enabled, the daily
// ignition hours, a loaded account, and ignitionTarget, which includes
// monitoring and ignition of its provider and the quota group.
func (e *Engine) mayIgniteLocked(t target) bool {
	cfg := e.cfg
	if !cfg.Ignition.Enabled || !e.schedule(cfg).InWindow(e.now()) {
		return false
	}
	group := e.groups[t.group.Key]
	return e.creds[t.cred.AuthIndex] != nil && group != nil && ignitionTarget(cfg, group)
}

// admission is the automatic ignition request being opened. Configure
// withdraws it when the new settings no longer let its target ignite, and
// cancel stops a request that is about to be sent or waits for its
// response headers. Once the headers arrive the request has reached the
// provider and is no longer withdrawn.
type admission struct {
	target    target
	cancel    context.CancelCauseFunc
	withdrawn bool
}

// admit checks under mu that the automatic ignition of t may send its next
// request, and registers it as e.admission, so that a configuration
// applied by Configure after the check withdraws it. The returned context
// carries the request; opened ends the admission once OpenModelStream has
// returned and reports false when the request was withdrawn meanwhile.
// A stopped engine admits nothing, and Cancel cancels the admitted request.
func (e *Engine) admit(ctx context.Context, t target) (context.Context, func() bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || !e.mayIgniteLocked(t) {
		return ctx, nil, false
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	a := &admission{target: t, cancel: cancel}
	e.admission = a
	opened := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.admission == a {
			e.admission = nil
		}
		return !a.withdrawn
	}
	return reqCtx, opened, true
}

// withdrawLocked withdraws the admitted request when the settings just
// applied no longer let its target ignite. The caller holds mu.
func (e *Engine) withdrawLocked() {
	a := e.admission
	if a == nil || a.withdrawn || e.mayIgniteLocked(a.target) {
		return
	}
	a.withdrawn = true
	a.cancel(errIgnitionWithdrawn)
}

// configuredModel returns the model override for the target, if any.
func configuredModel(cfg config.Config, provider, groupLabel string) string {
	p := cfg.Provider(provider)
	for key, value := range p.Models {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(groupLabel)) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return p.Model
}

// gatewayModels holds the models of one Magpie provider from the gateway's
// list. Magpie names a model "<provider>/<model>", such as
// "claude/claude-haiku-4-5" or "agy-cli/gemini-3-flash"; the candidate rules
// of internal/providers read the model name alone. bare lists the names, and
// ids maps each back to the id the request names. An id without a
// provider is kept as it is.
type gatewayModels struct {
	bare []string
	ids  map[string]string
}

// modelsFor returns the models in available whose provider part is accepted
// by owns.
func modelsFor(available []string, owns func(provider string) bool) gatewayModels {
	m := gatewayModels{ids: map[string]string{}}
	for _, id := range available {
		name := id
		if provider, rest, ok := strings.Cut(id, "/"); ok {
			if !owns(provider) {
				continue
			}
			name = rest
		}
		if _, seen := m.ids[name]; !seen && name != "" {
			m.ids[name] = id
			m.bare = append(m.bare, name)
		}
	}
	return m
}

// credModels returns the models of the Magpie provider of cred. A request
// pinned to an account can only use its own provider's models.
func credModels(available []string, cred *Cred) gatewayModels {
	return modelsFor(available, func(provider string) bool {
		if cred.HostProvider != "" {
			return strings.EqualFold(provider, cred.HostProvider)
		}
		p := providers.Lookup(provider)
		return p != nil && p.ID == cred.Provider
	})
}

// id returns the gateway id of a model name.
func (m gatewayModels) id(name string) string {
	if id, ok := m.ids[name]; ok {
		return id
	}
	return name
}

// overrideName returns a configured model as a name of cred's provider: a
// value written as "<provider>/<model>" loses its provider part when it is
// cred's provider.
func overrideName(override string, cred *Cred) string {
	if provider, rest, ok := strings.Cut(override, "/"); ok && cred.HostProvider != "" && strings.EqualFold(provider, cred.HostProvider) {
		return rest
	}
	return override
}

// overrideID returns the id a configured model is sent as when the model
// list cannot be read.
func overrideID(override string, cred *Cred) string {
	if strings.Contains(override, "/") || cred.HostProvider == "" {
		return override
	}
	return cred.HostProvider + "/" + override
}

// igniteTarget sends the ignition request, confirms it and records the
// outcome. It returns the error shown for a manual ignition.
func (e *Engine) igniteTarget(ctx context.Context, cfg config.Config, t target, manual bool) error {
	ts := e.state.Target(t.group.Key)
	schedule := e.schedule(cfg)
	start := e.now()
	ts.LastAttemptEpoch = float64(start.UnixNano()) / 1e9
	e.dirty = true
	// The fingerprint of the credential the request goes out with; a
	// rejection waits for another one. When it cannot be read, a rejection
	// keeps it unknown: the file read after the request may already hold a
	// new login the request did not use. See noteFingerprint. For an
	// agy-cli account it is the sign-in confirmed before the last request
	// that went out (sentResponse.fingerprint).
	//
	// The gateway sends an agy-cli account's request with whichever Google
	// account its agy is signed in to. The account is confirmed before
	// every ignition, manual ones too, and again before each request
	// (execute); an account that cannot be confirmed gets no request. See
	// identityChecked.
	fingerprint := ""
	var err error
	if !manual || identityChecked(t.cred) {
		fingerprint, err = e.ignitionCredential(ctx, t.cred)
	}
	base := e.ignitionBase(t.group.Key, start)

	var model string
	var resp sentResponse
	if err == nil {
		model, resp, err = e.execute(ctx, cfg, t, manual)
	}
	if err == nil {
		// Confirmation needs a reading taken after the request that
		// worked was sent; the model list and refused candidates before it
		// can take a while.
		if resp.sentAt.After(base.start) {
			base.start = resp.sentAt
		}
		err = e.confirm(ctx, cfg, t, base, resp.ModelResponse, manual)
	}
	if resp.confirmed {
		// The sign-in confirmed right before the last request that went
		// out, which agy may have changed since the first confirmation.
		fingerprint = resp.fingerprint
	}
	if errors.Is(err, host.ErrIdentity) {
		// An unconfirmed sign-in has no fingerprint to wait on; the first
		// readable credential counts as changed (noteFingerprint).
		fingerprint = ""
	}
	now := e.now()
	if errors.Is(err, errIgnitionWithdrawn) {
		// Not a failure: the target no longer qualifies for automatic
		// ignition, and the next round reads the settings again.
		e.logf("info", "%s：%v", t.group.Label, err)
		return err
	}
	if (e.stopping() || ctx.Err() != nil) && errors.Is(err, context.Canceled) {
		// The process exits: the model list, the request, its stream or
		// its confirmation was cut short by the stop and says nothing about
		// the account. An error the provider had already answered is not
		// context.Canceled and is handled below.
		e.logf("info", "%s：退出时取消点火，不计为失败", t.group.Label)
		return err
	}
	if err != nil {
		d := failure.Diagnose(err)
		if errors.Is(err, host.ErrIdentity) {
			// A credential problem: retrying cannot help until the account
			// can be confirmed, and no provider limited anything.
			d.Reason = failure.ReasonAuth
		}
		if manual {
			// Keep the automatic retry, circuit and credential state
			// unchanged. manual=1 without retry_seconds tells the page that
			// no retry follows.
			e.addEventDetail("warn", "ignite_failed", t.group.Key, t.group.Label,
				err.Error(), fmt.Sprintf("%s 手动点火失败：%v", t.group.Label, err),
				map[string]string{"manual": "1", "error": err.Error(), "summary": d.Summary})
			return err
		}
		if d.Reason == failure.ReasonAuth || d.Reason == failure.ReasonSuspended {
			// Retrying cannot help: ignition waits for a new credential or a
			// successful manual ignition.
			ignite.RecordCredentialFailure(ts, now, err, d.Summary)
			e.dirty = true
			wait := map[string]string{failure.ReasonAuth: "等待更新凭证", failure.ReasonSuspended: "账号已停用"}[d.Reason]
			e.addEventDetail("error", "ignite_paused", t.group.Key, t.group.Label,
				fmt.Sprintf("暂停，%s：%s", wait, d.Summary),
				fmt.Sprintf("%s 点火失败，暂停，%s：%v", t.group.Label, wait, err),
				map[string]string{"error": err.Error(), "summary": d.Summary, "reason": d.Reason})
			e.noteCredentialProblem(ctx, t.cred, t.group.Key, d, sourceIgnite, fingerprint)
			return err
		}
		outcome := schedule.RecordFailure(ts, now, err)
		e.dirty = true
		if outcome.CircuitOpened {
			until := outcome.Until.In(cfg.Location()).Format("01/02 15:04")
			params := map[string]string{"until": eventTime(outcome.Until), "error": err.Error(), "summary": d.Summary}
			if d.Reason != "" {
				params["reason"] = d.Reason
			}
			e.addEventDetail("error", "ignite_paused", t.group.Key, t.group.Label,
				fmt.Sprintf("暂停到 %s：%s", until, d.Summary),
				fmt.Sprintf("%s 点火失败，暂停到 %s：%v", t.group.Label, until, err), params)
			if d.Reason == failure.ReasonRateLimited && outcome.NotifyCircuit {
				e.credentialEvent(t.cred, t.group.Key, d, sourceIgnite, outcome.Until)
			}
			if outcome.NotifyCircuit && e.alerts != nil && e.alerts.Sender != nil {
				msg := notify.CircuitMessage(e.language(), t.group.Label, outcome.Until, d.Summary, cfg.Location())
				if d.Reason == failure.ReasonRateLimited {
					msg = rateLimitedNotice(e.language(), t.group.Label, d, outcome.Until, cfg.Location())
				}
				switch sendErr := e.alerts.Sender.Send(ctx, msg); {
				case errors.Is(sendErr, notify.ErrNotConfigured):
					// Notifications are off.
				case sendErr != nil:
					e.notifyFailed(t.group.Key, msg, sendErr)
				default:
					e.notified(t.group.Key, msg)
				}
			}
		} else {
			retry := retryText(outcome.RetryIn)
			if outcome.Cooldown {
				retry = "Magpie 账号休息结束，" + retry
			}
			params := map[string]string{"retry_seconds": strconv.Itoa(int(outcome.RetryIn.Round(time.Second).Seconds())), "error": err.Error(), "summary": d.Summary}
			if outcome.Cooldown {
				params["cooldown"] = "1"
			}
			e.addEventDetail("warn", "ignite_failed", t.group.Key, t.group.Label,
				fmt.Sprintf("%s后重试：%v", retry, err),
				fmt.Sprintf("%s 点火失败，%s后重试：%v", t.group.Label, retry, err), params)
		}
		return err
	}
	ignite.RecordSuccess(ts, model, now)
	e.dirty = true
	e.noteCredentialOK(ctx, t.cred, sourceIgnite)
	resetText := "未知"
	params := map[string]string{"model": model}
	if group, ok := e.group(t.group.Key); ok {
		if w, ok := group.fiveHour(); ok && !w.Reset.IsZero() {
			resetText = w.Reset.In(cfg.Location()).Format("01/02 15:04:05")
			params["reset"] = eventTime(w.Reset)
		}
	}
	kind := "ignite"
	if manual {
		kind = "ignite_manual"
	}
	e.addEventDetail("info", kind, t.group.Key, t.group.Label,
		fmt.Sprintf("%s · 下次重置 %s", model, resetText),
		fmt.Sprintf("%s 点火成功（%s），下次重置 %s", t.group.Label, model, resetText), params)
	return nil
}

func (e *Engine) group(key string) (GroupView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	group, ok := e.groups[key]
	if !ok {
		return GroupView{}, false
	}
	snapshot := *group
	snapshot.Windows = append([]WindowView(nil), group.Windows...)
	return snapshot, true
}

// execute sends the ignition request with the newest candidate model. It
// tries the next candidate when Magpie refuses a model for the pinned
// account, and once per ignition when the provider refuses a model. The
// response carries when the returned candidate was sent, and for an
// agy-cli account the sign-in confirmed before the last request that went
// out, also with an error.
//
// Each request of an automatic ignition passes admit, which checks that
// the target still qualifies; otherwise the requests not sent yet are
// dropped with errIgnitionWithdrawn. Each request of an agy-cli account
// confirms the account right before it is sent (confirmIdentity): agy can
// sign in to another account while the model list is read or a candidate
// is refused. An account not confirmed before a later candidate returns
// an error that names that candidate, as the ones before it went out.
//
// A model list that fails because the engine stops or ctx ends returns
// context.Canceled.
func (e *Engine) execute(ctx context.Context, cfg config.Config, t target, manual bool) (string, sentResponse, error) {
	var empty sentResponse
	t.automatic = !manual
	spec := providers.Lookup(t.group.Provider)
	override := configuredModel(cfg, t.group.Provider, t.group.SourceLabel)
	ts := e.state.Target(t.group.Key)

	available, err := e.lister.List(ctx, e.host.Models)
	if err != nil && (ctx.Err() != nil || e.stopping()) {
		// The host's error need not tell that the stop cut it short.
		return "", empty, context.Canceled
	}
	e.noteModels(available, err)
	var candidates []string
	switch {
	case err != nil && override != "":
		candidates = []string{overrideID(override, t.cred)}
	case err != nil && ts.LastModel != "":
		// Keep igniting with the last working model while the list is unavailable.
		candidates = []string{ts.LastModel}
	case err != nil:
		return "", empty, err
	default:
		// Candidates are picked by model name and sent by gateway id.
		own := credModels(available, t.cred)
		names, err := providers.Candidates(spec, t.group.SourceLabel, overrideName(override, t.cred), own.bare)
		if err != nil {
			return "", empty, fmt.Errorf("%w: %v", ignite.ErrNoModel, err)
		}
		for _, name := range names {
			candidates = append(candidates, own.id(name))
		}
	}
	if override == "" {
		candidates = e.skipRefused(t.group.Key, candidates)
	}
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	var lastErr, refusedErr error
	refused := ""
	// sent is the sign-in of the last request that went out, for the
	// returns after the loop.
	var sent sentResponse
	for i, model := range candidates {
		if ctx.Err() != nil || e.stopping() {
			return model, empty, context.Canceled
		}
		protocol, body, err := providers.Request(spec, model)
		if err != nil {
			return "", empty, err
		}
		fingerprint := ""
		if identityChecked(t.cred) {
			if fingerprint, err = e.confirmIdentity(ctx, t.cred, i+1); err != nil {
				return model, empty, err
			}
		}
		resp, err := e.executeStream(ctx, t, host.ModelRequest{
			Account:  t.cred.AuthIndex,
			Protocol: protocol,
			Model:    model,
			Body:     body,
		}, ignitionOutputWindow)
		resp.confirmed, resp.fingerprint = identityChecked(t.cred), fingerprint
		sent = sentResponse{confirmed: resp.confirmed, fingerprint: fingerprint}
		if err == nil {
			if refused != "" {
				e.noteRefused(t, refused, model)
			}
			return model, resp, nil
		}
		if errors.Is(err, errIgnitionWithdrawn) {
			return model, resp, err
		}
		if ignite.Classify(err) == ignite.Unsupported {
			lastErr = err
			continue
		}
		// Magpie lists a new model for every account of the provider, and one
		// account may not be able to use it yet. A second refusal points at
		// the request or the account, so it ends the ignition.
		if refused == "" && i < len(candidates)-1 && ignite.ModelRejected(err) {
			refused, refusedErr = model, err
			continue
		}
		if refused != "" {
			// Only the status of the refusal is added: its text could carry
			// markers that change how the final error is classified.
			status := ""
			if code := host.StatusOf(refusedErr); code != 0 {
				status = fmt.Sprintf("（状态码 %d）", code)
			}
			err = fmt.Errorf("%w；此前 %s 被上游拒绝%s", err, refused, status)
		}
		return model, resp, err
	}
	if refused != "" {
		return "", sent, fmt.Errorf("%w（%s 被上游拒绝：%v；最后一次：%v）", ignite.ErrNoModel, refused, refusedErr, lastErr)
	}
	return "", sent, fmt.Errorf("%w（最后一次：%v）", ignite.ErrNoModel, lastErr)
}

// refusedFor is how long a model the provider refused for a quota group is
// skipped. A new model can reach accounts in stages, so it is tried again
// after a day instead of being dropped; a day keeps requests for a model the
// account cannot use rare.
const refusedFor = 24 * time.Hour

// noteRefused remembers that the provider refused model for the target while
// fallback worked, which shows the refusal concerns the model.
func (e *Engine) noteRefused(t target, model, fallback string) {
	if e.refused == nil {
		e.refused = map[string]time.Time{}
	}
	e.refused[t.group.Key+"\x00"+model] = e.now()
	hours := strconv.Itoa(int(refusedFor / time.Hour))
	e.addEventDetail("warn", "model_refused", t.group.Key, t.group.Label,
		fmt.Sprintf("%s 被上游拒绝，改用 %s，%s 小时内不再尝试", model, fallback, hours),
		fmt.Sprintf("%s 点火模型 %s 被上游拒绝，改用 %s，%s 小时内不再尝试", t.group.Label, model, fallback, hours),
		map[string]string{"model": model, "fallback": fallback, "hours": hours})
}

// skipRefused drops the candidates refused for the target within refusedFor,
// unless that leaves none, and forgets expired refusals.
func (e *Engine) skipRefused(key string, candidates []string) []string {
	now := e.now()
	for k, at := range e.refused {
		if now.Sub(at) >= refusedFor {
			delete(e.refused, k)
		}
	}
	var out []string
	for _, model := range candidates {
		if _, ok := e.refused[key+"\x00"+model]; !ok {
			out = append(out, model)
		}
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

// maybeReadModels reads the model list after a start, so that the settings
// show the next automatic model without waiting for an ignition. Otherwise
// the list is only read for an ignition. A failed read is retried with the
// next poll.
func (e *Engine) maybeReadModels(ctx context.Context, pollDue bool) {
	if e.modelsOK || (e.modelsTried && !pollDue) {
		return
	}
	e.modelsTried = true
	available, err := e.lister.List(ctx, e.host.Models)
	e.noteModels(available, err)
	e.modelsOK = err == nil
}

// noteModels records the result of reading the model list and the first
// automatic candidate of each provider. A failed read keeps the last known
// models.
func (e *Engine) noteModels(available []string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.modelsErr = err.Error()
		return
	}
	e.modelsErr = ""
	next := map[string]string{}
	for _, p := range providers.All() {
		if p.Ignition == nil {
			continue
		}
		own := modelsFor(available, func(provider string) bool { return providers.Lookup(provider) == p })
		if candidates, err := providers.Candidates(p, p.Ignition.HintGroup, "", own.bare); err == nil {
			next[p.ID] = own.id(candidates[0])
		}
	}
	e.nextModels = next
}

func truncateText(text string, n int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n])
}

// ignitionBase is what confirmation compares with: when the request that
// worked was sent, and the reset of the 5-hour window it is meant to
// replace.
type ignitionBase struct {
	// start is the start of the ignition until execute returns, then when
	// the request that worked entered OpenModelStream.
	start time.Time
	// oldReset is the reset of a window that had ended or not started at
	// start (a past reset or a rolling placeholder), zero otherwise. A
	// reading with this reset still describes that window.
	oldReset time.Time
}

// ignitionBase records the 5-hour window of key before an ignition at
// start.
func (e *Engine) ignitionBase(key string, start time.Time) ignitionBase {
	base := ignitionBase{start: start}
	group, ok := e.group(key)
	if !ok {
		return base
	}
	if w, ok := group.fiveHour(); ok && (e.state.Target(key).RollingReset || !w.Reset.After(start)) {
		base.oldReset = w.Reset
	}
	return base
}

// confirmed reports whether the target's 5-hour window has a fixed future
// reset read after the ignition request was sent (base.start), other than
// base.oldReset. The time of the reading counts, not when it was applied:
// Magpie's kept quota can hold a reading taken before the request and
// handed over after it.
func (e *Engine) confirmed(key string, base ignitionBase, now time.Time) bool {
	group, ok := e.group(key)
	if !ok {
		return false
	}
	w, ok := group.fiveHour()
	if !ok || w.ReadAt.Before(base.start) || w.Reset.IsZero() || !w.Reset.After(now) {
		return false
	}
	if !base.oldReset.IsZero() && w.Reset.Equal(base.oldReset) {
		return false
	}
	return !e.state.Target(key).RollingReset
}

// confirm checks that the ignition started a window. Claude responses carry
// the new reset in their headers, which Magpie's gateway writes back. The
// gateway does not pass on Codex's x-codex-* headers, and Antigravity has no
// quota in its headers: for them, and when the headers carry no new window,
// the credential is queried actively. The queries of a manual ignition
// leave the credential state alone, like its failure. A stop or an ended
// ctx before the window is confirmed returns context.Canceled, also when
// it cuts a query short.
func (e *Engine) confirm(ctx context.Context, cfg config.Config, t target, base ignitionBase, resp host.ModelResponse, manual bool) error {
	start := base.start
	var groups []quota.RawGroup
	if spec := providers.Lookup(t.group.Provider); spec != nil && spec.Headers != nil {
		groups = spec.Headers(resp.Header, e.now())
	}
	if len(groups) > 0 {
		now := e.now()
		e.passiveAt[t.cred.AuthIndex] = now
		e.notePassiveFable(t.cred, groups)
		e.applyGroups(ctx, cfg, t.cred, groups, quota.SourcePassive, now, start, true)
	}
	if e.confirmed(t.group.Key, base, e.now()) {
		return nil
	}
	for _, delay := range confirmDelays {
		if !e.waitWithUsage(ctx, delay) {
			return context.Canceled
		}
		if e.confirmed(t.group.Key, base, e.now()) {
			return nil
		}
		err := e.queryCred(ctx, cfg, t.cred, !manual)
		if e.confirmed(t.group.Key, base, e.now()) {
			return nil
		}
		if ctx.Err() != nil || e.stopping() {
			// The stop cut the query short, or came before the next one:
			// the window is neither confirmed nor refuted.
			return context.Canceled
		}
		if err != nil {
			e.logf("warn", "点火确认时刷新额度失败：%s：%v", t.group.Label, err)
		}
	}
	return ignite.ErrNotConfirmed
}

// retryText formats a retry delay in seconds below one minute, in minutes
// otherwise.
func retryText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Round(time.Second).Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Round(time.Minute).Minutes()))
}
