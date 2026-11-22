package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/notify"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// Cred is one monitored credential.
type Cred struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	// HostProvider is Magpie's provider id, which can be an alias of
	// Provider.
	HostProvider string `json:"host_provider,omitempty"`
	Email        string `json:"email,omitempty"`
	// User is Magpie's name of the account (host.Account.User).
	User     string `json:"user,omitempty"`
	Disabled bool   `json:"disabled"`
	// Note names the account: account_names, else Magpie's label when it
	// is not the email.
	Note string `json:"note,omitempty"`
	// Suffix tells accounts apart: "#" and the account name, such as
	// "#主力号", or the masked email of an unnamed account, such as "#rk".
	Suffix string `json:"suffix,omitempty"`
}

// WindowView is a quota window with its origin.
type WindowView struct {
	quota.Window
	Source quota.Source
	// ObservedAt is when the reading was applied.
	ObservedAt time.Time
	// ReadAt is when the provider produced the reading; see applyGroups.
	ReadAt time.Time
}

// GroupView is the current state of one quota group.
type GroupView struct {
	Key         string
	Provider    string
	AuthIndex   string
	SourceLabel string
	Label       string
	Windows     []WindowView
	// rolling and startKnown copy the ignition observation for model-safe reads.
	rolling    bool
	startKnown bool
}

func (g *GroupView) windows() []quota.Window {
	out := make([]quota.Window, 0, len(g.Windows))
	for _, w := range g.Windows {
		out = append(out, w.Window)
	}
	return out
}

func (g *GroupView) fiveHour() (WindowView, bool) {
	for _, w := range g.Windows {
		if quota.IsFiveHour(w.Window) {
			return w, true
		}
	}
	return WindowView{}, false
}

// usageEvent is a passive reading of the account authIndex taken at at.
type usageEvent struct {
	authIndex string
	groups    []quota.RawGroup
	at        time.Time
	// kept marks a reading from Magpie's kept quota. Magpie can take it
	// with an active query of its own, so unlike response headers it does
	// not show that a request started the 5-hour window.
	kept bool
}

type sampleMark struct {
	at        time.Time
	remaining float64
	reset     int64
}

// ObservePassive queues a passive reading of one account: groups read
// from Magpie's kept quota, or from the rate-limit headers of a response, at
// the time at. It returns at once; the loop applies the reading.
func (e *Engine) ObservePassive(pin string, groups []quota.RawGroup, at time.Time) {
	pin = strings.TrimSpace(pin)
	if pin == "" || len(groups) == 0 {
		return
	}
	if at.IsZero() {
		at = e.now()
	}
	select {
	case e.usageCh <- usageEvent{authIndex: pin, groups: groups, at: at}:
	default:
		// The loop is busy; dropping one passive sample is harmless.
	}
}

// pollKept reads Magpie's kept quotas and applies each account's reading
// that is newer than the last one applied. Magpie updates them from the
// rate-limit data of the requests it serves, so they are this plugin's
// passive data.
func (e *Engine) pollKept(ctx context.Context) {
	kept, err := e.host.KeptQuotas(ctx)
	if err != nil {
		e.logf("debug", "读取 Magpie 额度记录失败：%v", err)
		return
	}
	e.mu.Lock()
	known := make(map[string]bool, len(e.creds))
	for pin := range e.creds {
		known[pin] = true
	}
	e.mu.Unlock()
	for _, k := range kept {
		if !known[k.Pin] || k.ReadAt.IsZero() || !k.ReadAt.After(e.keptAt[k.Pin]) {
			continue
		}
		e.keptAt[k.Pin] = k.ReadAt
		e.applyUsage(ctx, usageEvent{authIndex: k.Pin, groups: k.Groups, at: k.ReadAt, kept: true})
	}
}

func (e *Engine) drainUsage(ctx context.Context) {
	for {
		select {
		case ev := <-e.usageCh:
			e.applyUsage(ctx, ev)
		default:
			return
		}
	}
}

func (e *Engine) applyUsage(ctx context.Context, ev usageEvent) {
	now := e.now()
	groups := ev.groups
	if len(groups) == 0 {
		return
	}
	e.mu.Lock()
	cred := e.creds[ev.authIndex]
	e.mu.Unlock()
	if cred == nil {
		return
	}
	cfg := e.config()
	if !cfg.Provider(cred.Provider).Monitor {
		return
	}
	readAt := ev.at
	if readAt.IsZero() {
		readAt = now
	}
	e.passiveAt[ev.authIndex] = now
	e.notePassiveFable(cred, groups)
	e.applyGroupsFrom(ctx, cfg, cred, groups, quota.SourcePassive, !ev.kept, now, readAt, true)
}

// notePassiveFable logs the first passive Fable window seen for a credential
// in this process. Regular Claude requests do not carry the 7d_oi headers;
// the log line shows whether a Fable request does.
func (e *Engine) notePassiveFable(cred *Cred, groups []quota.RawGroup) {
	for _, g := range groups {
		if g.Key != providers.FableGroupKey || e.fableSeen[cred.AuthIndex] {
			continue
		}
		if e.fableSeen == nil {
			e.fableSeen = map[string]bool{}
		}
		e.fableSeen[cred.AuthIndex] = true
		w := g.Windows[0]
		e.logf("info", "首次从响应头读到 Fable 额度：%s，剩余 %.1f%%，重置 %s",
			labelFor(providers.Title(cred.Provider), cred), w.Remaining, w.Reset.Format(time.RFC3339))
	}
}

func maskedSuffix(email string, ordinal int) string {
	local, _, _ := strings.Cut(email, "@")
	runes := []rune(local)
	switch {
	case len(runes) >= 2:
		return "#" + string(runes[len(runes)-2:])
	case len(runes) == 1:
		return "#" + local
	}
	return "#" + strconv.Itoa(ordinal)
}

// maxNameRunes is the longest account name; a longer one is cut and ends
// with "…".
const maxNameRunes = 20

// accountName returns the account name in a note: its first line, cut to
// maxNameRunes characters.
func accountName(note string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(note), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > maxNameRunes {
		return string(runes[:maxNameRunes]) + "…"
	}
	return string(runes)
}

// loadCreds reads the account list and assigns account suffixes. A named
// account gets "#" and its name. An unnamed account gets the masked email,
// unless it is the only monitored account of its provider. A named account
// whose suffix equals that of another account of its provider, such as two
// accounts with the same name, also gets the masked email.
func (e *Engine) loadCreds(ctx context.Context, cfg config.Config) ([]*Cred, error) {
	accounts, err := e.host.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	var creds []*Cred
	counts := map[string]int{}
	for _, account := range accounts {
		hostProvider := strings.ToLower(strings.TrimSpace(account.Provider))
		if !hostProviders[hostProvider] {
			if hostProvider != "" && account.On {
				e.logf("debug", "忽略 Magpie 服务 %s 的账号：不支持读取它的凭证", hostProvider)
			}
			continue
		}
		p := providers.Lookup(hostProvider)
		if p == nil || !account.On || strings.TrimSpace(account.Pin) == "" {
			continue
		}
		provider := p.ID
		if !cfg.Provider(provider).Monitor {
			continue
		}
		creds = append(creds, &Cred{
			ID:           account.Pin,
			AuthIndex:    account.Pin,
			Name:         account.Pin,
			Provider:     provider,
			HostProvider: hostProvider,
			Email:        account.Email,
			User:         account.User,
			Note:         accountNote(cfg, account),
		})
		counts[provider]++
	}
	ordinals := map[string]int{}
	masked := make([]string, len(creds))
	taken := map[[2]string]int{}
	for i, cred := range creds {
		ordinals[cred.Provider]++
		if counts[cred.Provider] > 1 {
			masked[i] = maskedSuffix(cred.Email, ordinals[cred.Provider])
		}
		cred.Suffix = masked[i]
		if name := accountName(cred.Note); name != "" {
			cred.Suffix = "#" + name
		}
		taken[[2]string{cred.Provider, cred.Suffix}]++
	}
	for i, cred := range creds {
		if accountName(cred.Note) != "" && taken[[2]string{cred.Provider, cred.Suffix}] > 1 {
			cred.Suffix += masked[i]
		}
	}
	byIndex := make(map[string]*Cred, len(creds))
	for _, cred := range creds {
		byIndex[cred.AuthIndex] = cred
	}
	e.mu.Lock()
	e.creds = byIndex
	for key, group := range e.groups {
		cred, ok := byIndex[group.AuthIndex]
		if !ok || cred.Provider != group.Provider {
			delete(e.groups, key)
			continue
		}
		group.Label = labelFor(group.SourceLabel, cred)
	}
	e.mu.Unlock()
	return creds, nil
}

// hostProviders are the Magpie providers whose accounts are monitored: the
// ones whose credentials package magpie reads. Magpie's own "antigravity"
// accounts map to the Antigravity provider too, but only the agy-cli
// credentials can be read, so they are left out.
var hostProviders = map[string]bool{"codex": true, "claude": true, "agy-cli": true}

// accountNote returns the name of account: account_names, else Magpie's
// label when the user named the account in Magpie. A label Magpie fills in
// itself, the email or the user ("a@b.com · Team"), is no name: it would
// show the full email in place of the masked label.
func accountNote(cfg config.Config, account host.Account) string {
	if name := cfg.AccountNames[account.Pin]; name != "" {
		return name
	}
	label := strings.TrimSpace(account.Label)
	if label == "" || strings.EqualFold(label, strings.TrimSpace(account.Email)) || strings.EqualFold(label, strings.TrimSpace(account.User)) {
		return ""
	}
	return label
}

// hostAccount is the host's view of cred.
func hostAccount(cred *Cred) host.Account {
	return host.Account{Pin: cred.AuthIndex, Provider: cred.HostProvider, Email: cred.Email, User: cred.User, On: true}
}

// shouldSkipActive reports whether the active query of cred can be skipped at
// slot because a passive update arrived shortly before it. Skipping stops
// after PassiveSkipMaxMinutes so that windows without passive data, such as
// Claude's Fable window, still refresh. Providers without quota in their
// response headers are never skipped.
func (e *Engine) shouldSkipActive(cfg config.Config, cred *Cred, slot, now time.Time) bool {
	if p := providers.Lookup(cred.Provider); p == nil || p.Headers == nil || cfg.PassiveSkipSeconds <= 0 {
		return false
	}
	passive, ok := e.passiveAt[cred.AuthIndex]
	if !ok || slot.Sub(passive) > time.Duration(cfg.PassiveSkipSeconds)*time.Second {
		return false
	}
	active, ok := e.activeAt[cred.AuthIndex]
	if !ok || now.Sub(active) >= time.Duration(cfg.PassiveSkipMaxMinutes)*time.Minute {
		return false
	}
	return true
}

// The time of a manual refresh. A tool call through Magpie waits at most
// about 50 seconds for the whole refresh (the plugin host gives it 55, and
// Lamplighter's tool route 50). One RefreshQuota of package magpie takes
// at most 8 seconds for its readings before the refresh and 32 for the
// refresh, in which Magpie's gateway waits at most 30 seconds for one
// quota. Tests shorten them.
var (
	// manualRefreshBudget bounds one manual refresh from its start, the
	// account list included: the refreshes that Magpie runs for expired
	// credentials share it, and end keptRereadWait before it, so that the
	// kept quota can still be read after a refresh that ran out.
	manualRefreshBudget = 48 * time.Second
	// keptRereadWait bounds reading Magpie's kept quota after a refresh
	// whose answer brought no card of the account to apply.
	keptRereadWait = 5 * time.Second
	// replyMargin is left between the end of a manual refresh and the
	// deadline of the request that asked for it, for the reply.
	replyMargin = time.Second
)

// A scheduled round has Magpie refresh the quota of an account whose token
// has expired, at most once per scheduledRenewGap. Magpie keeps no reading
// of its own for an idle account, so the kept quota would stay as old as
// the token. A Claude refresh runs Claude Code's /usage in the account's
// configuration directory, which also renews the token, so later rounds
// query Anthropic directly again: an idle Claude account runs Claude Code
// about once per token lifetime (8 hours). The gap bounds the runs while
// a refresh keeps failing. scheduledRenewTimeout covers what one refresh
// of package magpie takes (refreshLookTimeout and refreshTimeout).
var (
	scheduledRenewGap     = 30 * time.Minute
	scheduledRenewTimeout = 45 * time.Second
)

// renewDue reports whether a scheduled round may have Magpie refresh the
// quota of cred now, and if so records the refresh as tried.
func (e *Engine) renewDue(cred *Cred) bool {
	now := e.now()
	if last, ok := e.renewedAt[cred.AuthIndex]; ok && now.Sub(last) < scheduledRenewGap {
		return false
	}
	e.renewedAt[cred.AuthIndex] = now
	return true
}

// manualRefreshEnd is when a manual refresh started at start ends: after
// manualRefreshBudget, or replyMargin before deadline, the deadline of the
// request that asked for it (zero for none), when that is sooner.
func manualRefreshEnd(start, deadline time.Time) time.Time {
	end := start.Add(manualRefreshBudget)
	if !deadline.IsZero() && deadline.Add(-replyMargin).Before(end) {
		end = deadline.Add(-replyMargin)
	}
	return end
}

// poll runs one round of active queries. slot is the aligned time this round
// belongs to. onlyAuth limits the round to one credential. force disables the
// passive skip rule, for the reset probes.
func (e *Engine) poll(ctx context.Context, cfg config.Config, slot time.Time, onlyAuth string, force bool) {
	e.pollWith(ctx, cfg, slot, onlyAuth, force, nil)
}

// manualPoll is the round of a manual refresh (the refresh button, POST
// /api/refresh and the refresh_quotas tool): forced, and an expired
// credential first has Magpie refresh its quota (refreshKept). Scheduled
// rounds never do: a Claude refresh runs Claude Code's /usage.
//
// The whole round, the account list, the active queries and the
// readings of Magpie's kept quota included, ends at the end of the manual
// refresh (manualRefreshEnd): deadline is that of the request that asked
// for the refresh, zero for none. The accounts not queried by then fail
// with errManualTimeUp. The refreshes share renew, which ends
// keptRereadWait before the end, so that the kept quota can still be read
// after a refresh that ran out.
//
// It returns the error of the refresh: for one account, that account's
// error, or errRefreshAccountGone when the account is not loaded; for all,
// only a failed account list. A refreshFailure in it gives the reason
// Magpie could not refresh, or brought no new reading.
func (e *Engine) manualPoll(ctx context.Context, cfg config.Config, onlyAuth string, deadline time.Time) error {
	end := manualRefreshEnd(time.Now(), deadline)
	round, cancelRound := context.WithDeadlineCause(ctx, end, errManualTimeUp)
	defer cancelRound()
	renew, cancel := context.WithDeadlineCause(round, end.Add(-keptRereadWait), errManualTimeUp)
	defer cancel()
	errs, listErr := e.pollWith(round, cfg, e.now(), onlyAuth, true, renew)
	switch {
	case listErr != nil:
		return listErr
	case onlyAuth == "":
		return nil
	case errs[onlyAuth] != nil:
		return errs[onlyAuth]
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.creds[onlyAuth] == nil && ctx.Err() == nil {
		// Magpie no longer lists the account as on, or its provider is
		// not monitored.
		return errRefreshAccountGone
	}
	return nil
}

// refreshFailure is a manual refresh that Magpie could not run, with a
// reason that the results of POST /api/refresh and refresh_quotas show as
// it is: a short sentence without account names, emails or Magpie's text.
// A stale refresh ran but brought no new reading; its reason is a whole
// sentence ("Magpie 这次没有读到新额度：…") and may end with the short
// cause that package magpie gives with refresh_stale.
type refreshFailure struct {
	reason string
	err    error
	stale  bool
}

func (f *refreshFailure) Error() string { return f.reason }
func (f *refreshFailure) Unwrap() error { return f.err }

var errRefreshAccountGone = &refreshFailure{reason: "没有找到这个账号：它在 Magpie 中已关闭或已删除，或所属服务未开启监控"}

// errManualTimeUp is the cause of the contexts of a manual refresh that
// reach its end (manualRefreshEnd), and the error of an account the
// refresh had no time left to query.
var errManualTimeUp = &refreshFailure{reason: "超过手动刷新的时限，未完成"}

// timeUp reports whether ctx ended at the end of a manual refresh, not by
// a stop.
func timeUp(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), errManualTimeUp)
}

// refreshReasons are the reasons of the host.Error codes RefreshQuota
// returns when Magpie cannot refresh the account.
var refreshReasons = map[string]string{
	"account_off":         "账号在 Magpie 中已关闭",
	"account_not_found":   "Magpie 中找不到这个账号",
	"gateway_unavailable": "Magpie 网关不可用",
}

// refreshStaleCode is the host.Error code of a refresh that Magpie ran
// without a new reading: the account's quota card has an error, is
// deferred, or was not read again. Its Message is a short cause without
// emails or keys.
const refreshStaleCode = "refresh_stale"

// staleRefresh returns the refreshFailure of a refresh that brought no new
// reading, with cause when there is one.
func staleRefresh(cause string, err error) *refreshFailure {
	reason := "Magpie 这次没有读到新额度"
	if cause = truncateText(cause, 120); cause != "" {
		reason += "：" + cause
	}
	return &refreshFailure{reason: reason, err: err, stale: true}
}

// pollWith is poll; a non-nil renew lets an expired credential have Magpie
// refresh its quota, within renew's deadline. It returns the error of each
// account queried, or the error of the account list.
func (e *Engine) pollWith(ctx context.Context, cfg config.Config, slot time.Time, onlyAuth string, force bool, renew context.Context) (map[string]error, error) {
	start := e.now()
	creds, err := e.loadCreds(ctx, cfg)
	if err != nil {
		err = fmt.Errorf("读取凭证列表失败：%w", err)
		e.mu.Lock()
		e.pollErrs = map[string]string{"": err.Error()}
		e.mu.Unlock()
		e.logf("warn", "%v", err)
		return nil, err
	}
	errs := map[string]string{}
	failed := map[string]error{}
	for _, cred := range creds {
		if onlyAuth != "" && cred.AuthIndex != onlyAuth {
			continue
		}
		if timeUp(ctx) {
			// A manual refresh has no time left for this account.
			errs[cred.AuthIndex] = errManualTimeUp.Error()
			failed[cred.AuthIndex] = errManualTimeUp
			continue
		}
		if ctx.Err() != nil {
			return failed, nil
		}
		now := e.now()
		if !force && e.shouldSkipActive(cfg, cred, slot, now) {
			continue
		}
		if err := e.queryCredWith(ctx, cfg, cred, true, renew, renew == nil); err != nil {
			errs[cred.AuthIndex] = err.Error()
			failed[cred.AuthIndex] = err
			e.logf("warn", "额度刷新失败：%s：%v", labelFor(providers.Title(cred.Provider), cred), err)
		}
	}
	e.mu.Lock()
	if onlyAuth == "" {
		e.pollErrs = errs
		e.lastPoll = e.now()
	} else {
		delete(e.pollErrs, onlyAuth)
		for key, value := range errs {
			e.pollErrs[key] = value
		}
	}
	e.mu.Unlock()
	e.dirty = true
	if ctx.Err() == nil {
		// A manual refresh out of time leaves the notices to the next
		// round.
		e.flushCredentialNotices(ctx, start)
	}
	return failed, nil
}

// keptFallback applies Magpie's kept quota of cred, for an account whose
// token has expired. It is an error when Magpie has none.
func (e *Engine) keptFallback(ctx context.Context, cfg config.Config, cred *Cred) error {
	_, err := e.applyKept(ctx, cred)
	return err
}

// applyKept applies Magpie's kept quota of cred when it is newer than the
// last one applied, and returns when Magpie took it. It is an error when
// Magpie has none.
func (e *Engine) applyKept(ctx context.Context, cred *Cred) (time.Time, error) {
	kept, err := e.host.KeptQuotas(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("凭证已过期，读取 Magpie 额度记录失败：%w", err)
	}
	for _, k := range kept {
		if k.Pin == cred.AuthIndex {
			e.applyKeptQuota(ctx, k)
			return k.ReadAt, nil
		}
	}
	return time.Time{}, errors.New("凭证已过期，Magpie 也没有这个账号的额度记录")
}

// applyKeptQuota applies the kept reading k when it is newer than the last
// one applied to its account.
func (e *Engine) applyKeptQuota(ctx context.Context, k host.KeptQuota) {
	if k.ReadAt.After(e.keptAt[k.Pin]) {
		e.keptAt[k.Pin] = k.ReadAt
		e.applyUsage(ctx, usageEvent{authIndex: k.Pin, groups: k.Groups, at: k.ReadAt, kept: true})
	}
}

// refreshKept asks Magpie to read the quota of cred again, for an account
// whose token has expired, and then applies Magpie's new reading. Magpie's
// last reading applies in any case, for display, but the refresh only
// counts when it brought a new one: a failed refresh returns its error,
// and a refresh without a new reading (refresh_stale, or a kept reading no
// newer than the one applied before) a stale refreshFailure, each with the
// outcome of applying the last reading.
//
// A host.KeptRefresher answers the account's card with the refresh, which
// is applied as it is. It answers a card only when the card is newer than
// Magpie's reading from before the refresh, so the refresh counts only
// with a card: one that succeeds without a card is a format error. Without
// a host.KeptRefresher, and after a refresh that failed, the kept quota is
// read again within keptRereadWait, which manualPoll keeps free after
// renew; then the refresh counts when its reading is newer than the one
// applied before.
func (e *Engine) refreshKept(ctx, renew context.Context, cfg config.Config, cred *Cred) error {
	before := e.keptAt[cred.AuthIndex]
	var refreshErr error
	var card host.KeptQuota
	answered := false
	r, cards := e.host.(host.KeptRefresher)
	if cards {
		card, answered, refreshErr = r.RefreshKept(renew, cred.AuthIndex)
		answered = answered && card.Pin == cred.AuthIndex
		if refreshErr == nil && !answered {
			refreshErr = fmt.Errorf("%w: 刷新没有返回这个账号可用的额度卡", host.ErrFormat)
		}
	} else {
		refreshErr = e.host.RefreshQuota(renew, cred.AuthIndex)
	}
	var hostErr *host.Error
	switch {
	case errors.As(refreshErr, &hostErr) && hostErr.Code == refreshStaleCode:
		refreshErr = staleRefresh(hostErr.Message, refreshErr)
	case errors.As(refreshErr, &hostErr) && refreshReasons[hostErr.Code] != "":
		refreshErr = &refreshFailure{reason: refreshReasons[hostErr.Code], err: refreshErr}
	case refreshErr != nil && timeUp(renew):
		refreshErr = fmt.Errorf("超过手动刷新的时限，未完成：%w", refreshErr)
	}
	var readAt time.Time
	var err error
	if answered {
		e.applyKeptQuota(ctx, card)
		readAt = card.ReadAt
	} else {
		reread, cancel := context.WithTimeout(ctx, keptRereadWait)
		readAt, err = e.applyKept(reread, cred)
		cancel()
	}
	if !cards && refreshErr == nil && err == nil && !readAt.After(before) {
		// Magpie reported the refresh as done, but its reading is the one
		// applied before.
		refreshErr = staleRefresh("", nil)
	}
	var failure *refreshFailure
	stale := errors.As(refreshErr, &failure) && failure.stale
	switch {
	case refreshErr == nil:
		return err
	case stale && err != nil:
		return fmt.Errorf("%w；%w", refreshErr, err)
	case stale:
		return fmt.Errorf("%w；已显示 Magpie 记下的读数", refreshErr)
	case err != nil:
		return fmt.Errorf("Magpie 刷新额度失败：%w；%w", refreshErr, err)
	default:
		return fmt.Errorf("Magpie 刷新额度失败，已显示 Magpie 记下的读数：%w", refreshErr)
	}
}

// refreshCred runs the active query of one credential.
func (e *Engine) refreshCred(ctx context.Context, cfg config.Config, cred *Cred) error {
	return e.queryCredWith(ctx, cfg, cred, true, nil, false)
}

// queryCred runs the active query of one credential. Without track, the
// result does not change the credential state: no new or ended credential
// problem, event or notification.
func (e *Engine) queryCred(ctx context.Context, cfg config.Config, cred *Cred, track bool) error {
	return e.queryCredWith(ctx, cfg, cred, track, nil, false)
}

// queryCredWith is queryCred; with renew set, an expired credential has
// Magpie refresh its quota first (refreshKept). A scheduled round does the
// same at most once per scheduledRenewGap for each account (renewDue).
func (e *Engine) queryCredWith(ctx context.Context, cfg config.Config, cred *Cred, track bool, renew context.Context, scheduled bool) error {
	credential, err := e.host.Credential(ctx, hostAccount(cred))
	if err != nil {
		return fmt.Errorf("读取凭证失败：%w", err)
	}
	p := providers.Lookup(cred.Provider)
	if p == nil {
		return fmt.Errorf("不支持的额度 provider: %s", cred.Provider)
	}
	credential.Provider, credential.ID, credential.Name = cred.HostProvider, cred.ID, cred.Name
	fingerprint := credential.Fingerprint
	if track {
		e.noteFingerprint(cred, fingerprint)
	}
	if credential.Expired {
		// The token can only be renewed by Magpie or the agent CLI that
		// owns it; Magpie's own last reading stands in.
		if renew != nil {
			return e.refreshKept(ctx, renew, cfg, cred)
		}
		if scheduled && e.renewDue(cred) {
			scheduledRenew, cancel := context.WithTimeout(ctx, scheduledRenewTimeout)
			defer cancel()
			return e.refreshKept(ctx, scheduledRenew, cfg, cred)
		}
		return e.keptFallback(ctx, cfg, cred)
	}
	readAt := e.now()
	groups, err := providers.Fetch(ctx, p, e.doer, credential, readAt)
	if err != nil {
		if track {
			e.noteQueryFailure(ctx, cred, fingerprint, err)
		}
		return err
	}
	now := e.now()
	e.activeAt[cred.AuthIndex] = now
	if track {
		e.noteCredentialOK(ctx, cred, sourceQuery)
	}
	// The plan comes first, so that a changed plan resets the notification
	// baselines before the new windows are compared with them.
	confirmed := e.notePlan(ctx, p, cred, credential, groups, now)
	e.applyGroups(ctx, cfg, cred, groups, quota.SourceActive, now, readAt, confirmed)
	return nil
}

const (
	// planCheckPeriod is how often the plan of an account is requested on
	// its own, for providers whose usage response does not state it.
	planCheckPeriod = 24 * time.Hour
	// planRecheckPeriod is how long the plan is requested with every active
	// query after the windows of the account changed.
	planRecheckPeriod = time.Hour
	// planRetryPeriod is how long the next plan request waits after a failed
	// recheck, also once the recheck has ended. It goes out with the first
	// active query after that.
	planRetryPeriod = 15 * time.Minute
	// planNoticeAge is how long an undelivered plan change notification is
	// retried.
	planNoticeAge = time.Hour
)

// windowSet lists the quota groups and windows of an active result, sorted,
// to tell when an account starts or stops reporting a window.
func windowSet(groups []quota.RawGroup) string {
	var ids []string
	for _, g := range groups {
		for _, w := range g.Windows {
			ids = append(ids, g.Key+"|"+w.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\n")
}

// notePlan records the plan of cred after a successful quota query: from the
// usage response when it states one, otherwise from the provider's own
// request. That request is sent once per planCheckPeriod, and with every
// active query for planRecheckPeriod after the account reports other
// windows than before: an expired Google AI Pro plan, for one, removes the
// 5-hour windows of Antigravity. Rechecking covers a failed request and a
// plan that changes a little later than the windows. In the latter case the
// quota alerts of the queries before the change compare with the old
// baselines, so a false recovery can still be sent once. A failed request
// keeps the plan read before and does not affect the quota; after a failed
// recheck, the next request follows with the first active query
// planRetryPeriod later even when the recheck has ended. notePlan reports false when a recheck failed for
// an account with a known plan: the quota alerts of the account then wait
// for the next query, as the windows may belong to a plan not read yet.
func (e *Engine) notePlan(ctx context.Context, p *providers.Provider, cred *Cred, credential providers.Credential, groups []quota.RawGroup, now time.Time) bool {
	if p.Plan == nil {
		return true
	}
	state := e.state.PlanFor(cred.AuthIndex)
	if windows := windowSet(groups); windows != state.Windows {
		if state.Windows != "" {
			state.RecheckUntil = now.Add(planRecheckPeriod).Unix()
		}
		state.Windows = windows
		e.dirty = true
	}
	raw := ""
	for _, g := range groups {
		if g.Plan != "" {
			raw = g.Plan
		}
	}
	rechecking := now.Unix() < state.RecheckUntil
	confirmed := true
	if raw == "" && p.Profile != nil && (rechecking || now.Sub(time.Unix(state.Checked, 0)) >= planCheckPeriod) {
		state.Checked = now.Unix()
		e.dirty = true
		plan, err := p.Profile(ctx, e.doer, credential, now)
		if err != nil {
			e.logf("debug", "读取套餐失败：%s：%v", labelFor(providers.Title(cred.Provider), cred), err)
			if rechecking {
				// The daily request follows soon when the recheck ends
				// without a successful read.
				state.Checked = now.Add(planRetryPeriod - planCheckPeriod).Unix()
			}
			// Without a plan read before, a later read only records the plan
			// and leaves the baselines, so waiting would gain nothing.
			confirmed = !rechecking || state.Plan == ""
		}
		raw = plan
	}
	if raw != "" && raw != state.Plan {
		old := state.Plan
		state.Plan = raw
		e.dirty = true
		e.mu.Lock()
		e.plans[cred.AuthIndex] = raw
		e.mu.Unlock()
		from, to := providers.PlanOf(cred.Provider, old).Name, providers.PlanOf(cred.Provider, raw).Name
		if old != "" && from != to {
			state.RecheckUntil = 0
			e.planChanged(cred, state, from, to, now)
		}
	}
	e.sendPlanNotice(ctx, cred, state, now)
	return confirmed
}

// planChanged records a changed plan for its notification and marks the
// notification baselines of the account for a new start. The quota of the
// new plan is unrelated to the old one: a fresh 7-day quota on an expired
// plan is no recovery, and the quota that returns with a renewal is no drop.
// Like a window seen for the first time, each window then only records its
// baseline.
func (e *Engine) planChanged(cred *Cred, state *store.PlanState, from, to string, now time.Time) {
	prefix := cred.Provider + ":" + cred.AuthIndex + ":"
	for key, group := range e.state.Groups {
		if !strings.HasPrefix(key, prefix) || group == nil {
			continue
		}
		for _, ws := range group.Windows {
			if ws != nil {
				ws.Rebase = true
			}
		}
	}
	label := labelFor(providers.Title(cred.Provider), cred)
	e.addEventDetail("info", "plan_change", "", label, from+" → "+to, fmt.Sprintf("%s 套餐变化：%s → %s", label, from, to),
		map[string]string{"from": from, "to": to})
	// A change back before the first one was delivered cancels both.
	if n := state.Notice; n != nil {
		from = n.From
	}
	state.Notice = &store.PlanNotice{From: from, To: to, At: now.Unix()}
	if from == to {
		state.Notice = nil
	}
}

// sendPlanNotice sends the pending plan change notification of cred, and
// retries it with every active query for planNoticeAge after a failure.
func (e *Engine) sendPlanNotice(ctx context.Context, cred *Cred, state *store.PlanState, now time.Time) {
	n := state.Notice
	if n == nil || e.alerts == nil || e.alerts.Sender == nil {
		return
	}
	if now.Sub(time.Unix(n.At, 0)) > planNoticeAge {
		state.Notice = nil
		e.dirty = true
		return
	}
	msg := notify.PlanMessage(e.language(), labelFor(providers.Title(cred.Provider), cred), n.From, n.To)
	switch err := e.alerts.Sender.Send(ctx, msg); {
	case errors.Is(err, notify.ErrNotConfigured):
		// Notifications are off.
	case err != nil:
		e.notifyFailed("", msg, err)
		return
	default:
		e.notified("", msg)
	}
	state.Notice = nil
	e.dirty = true
}

// applyGroups merges new windows into the current view, records history,
// updates the reset observation of ignition targets and, when alerts is
// set, runs the alerts. An active result replaces the group; a passive result only updates the
// windows it contains. Once an active query has described the group, a
// passive result does not add windows: response headers can carry a window
// the usage API does not report, such as a 5-hour window on a ChatGPT Pro
// account whose usage API reports only the weekly one. A new window then
// appears with the next active query.
//
// readAt is when the provider produced the readings: the start of the
// active query, or the start of the request whose response headers carry
// them. A usage record arrives when its request completes, so with parallel
// requests an older reading can arrive after a newer one. A passive window
// read before the current one is dropped; its higher quota would otherwise
// count as a recovery and repeat the threshold alert just sent. A passive
// group whose windows are all dropped is not processed further.
func (e *Engine) applyGroups(ctx context.Context, cfg config.Config, cred *Cred, raw []quota.RawGroup, source quota.Source, now, readAt time.Time, alerts bool) {
	e.applyGroupsFrom(ctx, cfg, cred, raw, source, source == quota.SourcePassive, now, readAt, alerts)
}

// applyGroupsFrom is applyGroups. requested tells whether the readings come
// from the response of a request, which starts the 5-hour window: such a
// reading is never taken for a rolling placeholder (ignite.Observe). A
// passive reading from Magpie's kept quota is not requested, and is
// observed at readAt like an active one.
func (e *Engine) applyGroupsFrom(ctx context.Context, cfg config.Config, cred *Cred, raw []quota.RawGroup, source quota.Source, requested bool, now, readAt time.Time, alerts bool) {
	var changed []*GroupView
	previous := map[string][]WindowView{}
	e.mu.Lock()
	if source == quota.SourceActive {
		// A successful active response describes the whole credential. Keep empty
		// groups as tombstones so passive headers cannot resurrect retired pools.
		prefix := cred.Provider + ":" + cred.AuthIndex + ":"
		seen := map[string]bool{}
		for _, rg := range raw {
			seen[prefix+rg.Key] = true
		}
		addMissing := func(key, label string) {
			if strings.HasPrefix(key, prefix) && !seen[key] {
				raw = append(raw, quota.RawGroup{Key: strings.TrimPrefix(key, prefix), SourceLabel: label})
				seen[key] = true
			}
		}
		for key, g := range e.groups {
			addMissing(key, g.SourceLabel)
		}
		if e.state != nil {
			for key := range e.state.Groups {
				addMissing(key, "")
			}
		}
	}
	for _, rg := range raw {
		key := cred.Provider + ":" + cred.AuthIndex + ":" + rg.Key
		group, ok := e.groups[key]
		if !ok {
			group = &GroupView{Key: key, Provider: cred.Provider, AuthIndex: cred.AuthIndex}
			e.groups[key] = group
		}
		previous[key] = append([]WindowView(nil), group.Windows...)
		group.SourceLabel = rg.SourceLabel
		group.Label = labelFor(group.SourceLabel, cred)
		var activeWindows []string
		legacyUnconfirmed := false
		if e.state != nil {
			stored := e.state.Group(key)
			activeWindows = stored.ActiveWindows
			legacyUnconfirmed = stored.LegacyUnconfirmed
		}
		if source == quota.SourceActive {
			group.Windows = group.Windows[:0]
			activeWindows = make([]string, 0, len(rg.Windows))
			for _, w := range rg.Windows {
				activeWindows = append(activeWindows, w.ID)
			}
			if e.state != nil {
				stored := e.state.Group(key)
				if stored.LegacyUnconfirmed {
					// 无可靠存在信息的旧组，以首次主动结果重新建立通知基线。
					for _, ws := range stored.Windows {
						if ws != nil {
							ws.Rebase = true
						}
					}
				}
				stored.ActiveWindows = activeWindows
				stored.LegacyUnconfirmed = false
			}
		}
		stale := 0
		for _, w := range rg.Windows {
			if source == quota.SourcePassive && legacyUnconfirmed && activeWindows == nil && !slices.ContainsFunc(group.Windows, func(existing WindowView) bool { return existing.ID == w.ID }) {
				stale++
				continue
			}
			if source == quota.SourcePassive && activeWindows != nil && !slices.Contains(activeWindows, w.ID) {
				stale++
				continue
			}
			view := WindowView{Window: w, Source: source, ObservedAt: now, ReadAt: readAt}
			replaced := false
			for i := range group.Windows {
				if group.Windows[i].ID == w.ID {
					if source == quota.SourcePassive && readAt.Before(group.Windows[i].ReadAt) {
						stale++
					} else {
						group.Windows[i] = view
					}
					replaced = true
					break
				}
			}
			if !replaced {
				group.Windows = append(group.Windows, view)
			}
		}
		if stale > 0 && stale == len(rg.Windows) {
			continue
		}
		sort.SliceStable(group.Windows, func(i, j int) bool {
			return windowOrder(group.Windows[i].Window) < windowOrder(group.Windows[j].Window)
		})
		snapshot := *group
		snapshot.Windows = append([]WindowView(nil), group.Windows...)
		changed = append(changed, &snapshot)
	}
	e.mu.Unlock()

	for _, group := range changed {
		if source == quota.SourceActive {
			// Missing windows keep their baseline across restarts, but their
			// first reading after a return must not compare unrelated cycles.
			if stored := e.state.Groups[group.Key]; stored != nil {
				present := make(map[string]bool, len(group.Windows))
				for _, w := range group.Windows {
					present[w.ID] = true
				}
				for id, ws := range stored.Windows {
					if ws != nil && !present[id] {
						ws.Rebase = true
					}
				}
			}
		}
		if w, ok := group.fiveHour(); ok {
			target := e.state.Target(group.Key)
			seen := now
			if source == quota.SourcePassive && !requested {
				seen = readAt
			}
			startKnown := requested || target.ResetObservation != nil || w.Reset.Sub(seen) < quota.FiveHours-2*time.Minute
			ignite.Observe(target, w.Reset, seen, requested)
			e.mu.Lock()
			if live := e.groups[group.Key]; live != nil {
				live.rolling = target.RollingReset
				live.startKnown = startKnown
			}
			e.mu.Unlock()
		} else if source == quota.SourceActive {
			if target := e.state.Scheduler[group.Key]; target != nil {
				target.ResetObservation = nil
				target.RollingReset = false
			}
			e.mu.Lock()
			if live := e.groups[group.Key]; live != nil {
				live.startKnown = false
				live.rolling = false
			}
			e.mu.Unlock()
		}
		e.recordQuotaTransitions(group, previous[group.Key], now)
		e.recordSamples(group, source, now)
		if e.alerts != nil && alerts {
			e.alerts.ProcessGroup(ctx, e.state, notify.Group{Key: group.Key, Label: group.Label, Windows: group.windows()}, now)
		}
	}
	e.dirty = true
}

func windowOrder(w quota.Window) int {
	switch {
	case quota.IsFiveHour(w):
		return 0
	case w.ID == quota.WindowSevenDay:
		return 1
	case quota.IsSevenDay(w):
		return 2
	}
	return 3
}

// recordSamples writes history samples. Active samples are always written.
// Passive samples are written when the value changes, at most once per
// minute per window; a newer value inside that minute waits in pendingSamp.
func (e *Engine) recordSamples(group *GroupView, source quota.Source, now time.Time) {
	var records []store.Record
	for _, w := range group.Windows {
		if w.Source != source || !w.ObservedAt.Equal(now) {
			continue
		}
		key := group.Key + "|" + w.ID
		record := store.Record{
			Kind:      "s",
			Time:      now.Unix(),
			Group:     group.Key,
			Window:    w.ID,
			Remaining: w.Remaining,
			Source:    string(source),
		}
		if !w.Reset.IsZero() {
			record.Reset = w.Reset.Unix()
		}
		last, seen := e.lastSample[key]
		if source == quota.SourcePassive && seen {
			unchanged := last.remaining == record.Remaining && absInt64(last.reset-record.Reset) <= 10
			if unchanged {
				delete(e.pendingSamp, key)
				continue
			}
			if now.Sub(last.at) < time.Minute {
				e.pendingSamp[key] = record
				continue
			}
		}
		delete(e.pendingSamp, key)
		e.lastSample[key] = sampleMark{at: now, remaining: record.Remaining, reset: record.Reset}
		records = append(records, record)
	}
	if len(records) > 0 && e.history != nil {
		if err := e.history.Append(records...); err != nil {
			e.logf("warn", "写入历史记录失败：%v", err)
		}
	}
}

// flushSamples writes pending passive samples whose minute has passed, or
// all of them when force is set.
func (e *Engine) flushSamples(now time.Time, force bool) {
	var records []store.Record
	for key, record := range e.pendingSamp {
		last := e.lastSample[key]
		if !force && now.Sub(last.at) < time.Minute {
			continue
		}
		record.Time = now.Unix()
		e.lastSample[key] = sampleMark{at: now, remaining: record.Remaining, reset: record.Reset}
		records = append(records, record)
		delete(e.pendingSamp, key)
	}
	if len(records) > 0 && e.history != nil {
		e.history.Append(records...)
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (e *Engine) pollResetFeed(ctx context.Context, cfg config.Config) {
	records, err := notify.FetchResetRecords(ctx, &http.Client{Timeout: cfg.RequestTimeout()}, "magpie-lamplighter/"+e.version)
	if err != nil {
		e.logf("warn", "Did Codex Reset 检查失败：%v", err)
		return
	}
	var sender notify.Sender
	if e.alerts != nil && e.alerts.Sender != nil {
		sender = recordingSender{e: e, next: e.alerts.Sender}
	}
	sent := notify.ProcessResetRecords(ctx, e.state, records, cfg.CodexResetUpdates.NotifyCurrentPending, sender, cfg.Location(), e.language(), e.now())
	if sent > 0 {
		e.addEventDetail("info", "codex_reset", "", "Did Codex Reset", fmt.Sprintf("已发送 %d 条通知", sent), fmt.Sprintf("Did Codex Reset：已发送 %d 条通知", sent),
			map[string]string{"count": strconv.Itoa(sent)})
	}
	e.dirty = true
}

var errNoTarget = errors.New("找不到该点火目标")
