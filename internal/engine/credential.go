package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/notify"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/redact"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// Sources of a credential problem, the source param of its events.
const (
	sourceIgnite = "ignite"
	sourceQuery  = "query"
)

// A quota query can answer HTTP 401 for a token that has just expired and
// that Magpie or the agent CLI owning it has not renewed yet; Lamplighter
// never renews such a token itself, and the owner renews it at its own
// pace. A rejection that lasts queryProblemAge and queryProblemCount
// queries in a row is therefore taken as an invalid credential. A suspended
// account is reported with the first query.
const (
	queryProblemAge   = 30 * time.Minute
	queryProblemCount = 3
)

// credentialNoticeAge is how long an undelivered credential notification is
// retried with every poll.
const credentialNoticeAge = time.Hour

// Notice values of store.CredentialState.
const (
	noticeProblem  = "problem"
	noticeRestored = "restored"
)

func accountLabel(cred *Cred) string {
	return labelFor(providers.Title(cred.Provider), cred)
}

// accountGroup returns the key of the first quota group of the account, so
// that account events can be matched to the account; empty when the account
// has no group yet.
func (e *Engine) accountGroup(authIndex string) string {
	e.mu.Lock()
	var groups []*GroupView
	for _, g := range e.groups {
		if g.AuthIndex == authIndex {
			groups = append(groups, g)
		}
	}
	e.mu.Unlock()
	sort.SliceStable(groups, func(i, j int) bool {
		if ri, rj := providers.GroupRank(groups[i].SourceLabel), providers.GroupRank(groups[j].SourceLabel); ri != rj {
			return ri < rj
		}
		return groups[i].Key < groups[j].Key
	})
	if len(groups) > 0 {
		return groups[0].Key
	}
	return ""
}

// credentialBlocked reports whether ignition of the account waits for a new
// credential: the provider rejected it or disabled the account, and the
// credential has not changed since.
func (e *Engine) credentialBlocked(authIndex string) bool {
	if e.state == nil {
		return false
	}
	st := e.state.Credentials[authIndex]
	return st != nil && st.Reason != "" && !st.Changed
}

// credentialProblem returns the reason of the account's credential problem,
// empty when there is none.
func (e *Engine) credentialProblem(authIndex string) string {
	if e.state == nil {
		return ""
	}
	if st := e.state.Credentials[authIndex]; st != nil {
		return st.Reason
	}
	return ""
}

// publishCredential copies the problem reason of an account for Status.
func (e *Engine) publishCredential(authIndex, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.credProblems == nil {
		e.credProblems = map[string]string{}
	}
	if reason == "" {
		delete(e.credProblems, authIndex)
	} else {
		e.credProblems[authIndex] = reason
	}
}

// noteFingerprint marks the credential problem of cred as changed when the
// credential file holds another login than the one the problem was found
// with. Ignition then tries again. A problem recorded while the file could
// not be read has no fingerprint to compare; the first readable file counts
// as changed, so that ignition tries once and records the fingerprint if it
// fails again.
func (e *Engine) noteFingerprint(cred *Cred, fingerprint string) {
	if e.state == nil {
		return
	}
	st := e.state.Credentials[cred.AuthIndex]
	if st == nil || st.Reason == "" || st.Changed || fingerprint == "" || fingerprint == st.Fingerprint {
		return
	}
	st.Changed = true
	e.dirty = true
	if st.Fingerprint == "" {
		e.logf("info", "%s 的凭证在出现问题时无法读取，现在可以读取，恢复自动点火", accountLabel(cred))
		return
	}
	e.logf("info", "%s 的凭证已更新，恢复自动点火", accountLabel(cred))
}

// agyCLI is Magpie's provider id of the agy-cli plugin.
const agyCLI = "agy-cli"

// identityChecked reports whether ignition of cred confirms the account
// before every request, manual ignition too. The gateway sends an agy-cli
// account's requests through agy, which uses whichever Google account it
// is signed in to now; the pin alone does not tell which. Codex and Claude
// requests use the account's own credential.
func identityChecked(cred *Cred) bool {
	return strings.EqualFold(cred.HostProvider, agyCLI)
}

// errIdentityUnsent prefixes an ignition refused for host.ErrIdentity
// before any of its requests was sent.
const errIdentityUnsent = "没有发送点火请求"

// errNoIdentityChecker is why an agy-cli account is not confirmed by a
// host that does not implement host.IdentityChecker: Credential alone
// cannot tell that the gateway will use the account's own sign-in.
var errNoIdentityChecker = errors.New("the host cannot confirm the sign-in of an agy-cli account")

// identityUnconfirmed is an ignition request not sent because the
// account could not be confirmed. It wraps host.ErrIdentity and the
// host's error, but its text gives only identityReason.
type identityUnconfirmed struct {
	err error
	// candidate is the number of the candidate model that was not sent,
	// from 1; 0 or 1 means that the ignition sent no request.
	candidate int
}

func (u identityUnconfirmed) Error() string {
	unsent := errIdentityUnsent
	if u.candidate > 1 {
		unsent = fmt.Sprintf("第 %d 个候选模型未发送", u.candidate)
	}
	return unsent + "：" + identityReason(u.err)
}

func (u identityUnconfirmed) Unwrap() []error { return []error{host.ErrIdentity, u.err} }

// identityReason is why the account of an ignition was not confirmed, as
// events, logs and API answers show it. A host.ErrIdentity error gives the
// host's own reason, which names the other account by a masked email
// (redactDetail masks any other). The host's other errors can quote the
// fields of a credential file, so they are told by kind alone.
func identityReason(err error) string {
	const unconfirmed = "无法确认账号身份："
	switch {
	case errors.Is(err, errNoIdentityChecker):
		return unconfirmed + "宿主不支持核对 agy 账号的身份"
	case errors.Is(err, host.ErrIdentity):
		return redactDetail(err.Error())
	case errors.Is(err, host.ErrFormat):
		return unconfirmed + "凭证文件格式不对"
	case errors.Is(err, fs.ErrNotExist):
		return unconfirmed + "找不到凭证文件"
	}
	return unconfirmed + "读取凭证失败"
}

// detailLimit bounds the text redactDetail keeps.
const detailLimit = 200

var (
	detailEmail  = regexp.MustCompile(`[^\s@"'<>()\[\],;:（）：，；“”]+@[^\s@"'<>()\[\],;:（）：，；“”]+`)
	detailQuoted = regexp.MustCompile(`"[^"]*"`)
	detailSecret = regexp.MustCompile(`[A-Za-z0-9_\-./+=]{32,}`)
)

// redactDetail is the text of a host error, fit to show or log: one line,
// emails masked (maskEmail), quoted values and long token-like strings
// left out, at most detailLimit characters.
func redactDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = detailEmail.ReplaceAllStringFunc(s, maskEmail)
	s = detailQuoted.ReplaceAllString(s, `"…"`)
	s = detailSecret.ReplaceAllString(s, "…")
	if r := []rune(s); len(r) > detailLimit {
		s = string(r[:detailLimit]) + "…"
	}
	return s
}

// unconfirmed is identityUnconfirmed for err, the error of confirming cred
// before the candidate-th candidate model (0 before the model list). The
// host's error is logged at debug level, redacted (redactDetail).
func (e *Engine) unconfirmed(cred *Cred, candidate int, err error) error {
	e.logf("debug", "%s：无法确认账号身份：%s", accountLabel(cred), redactDetail(err.Error()))
	return identityUnconfirmed{err: err, candidate: candidate}
}

// confirmIdentity confirms that the gateway sends cred's requests with
// cred's own sign-in, and returns the sign-in's fingerprint, before the
// candidate-th candidate model (0 before the model list). Only a
// host.IdentityChecker can confirm it, from local files alone; a host
// without one confirms no agy-cli account. Any error means the account is
// not confirmed: it is returned as identityUnconfirmed, and no request may
// be sent.
func (e *Engine) confirmIdentity(ctx context.Context, cred *Cred, candidate int) (string, error) {
	checker, ok := e.host.(host.IdentityChecker)
	if !ok {
		return "", e.unconfirmed(cred, candidate, errNoIdentityChecker)
	}
	fingerprint, err := checker.ConfirmIdentity(ctx, hostAccount(cred))
	if err != nil {
		return "", e.unconfirmed(cred, candidate, err)
	}
	return fingerprint, nil
}

// ignitionCredential reads the fingerprint of cred's credential before an
// ignition, "" when it cannot be read. An agy-cli account
// (identityChecked) must be confirmed (confirmIdentity), and any error
// refuses the ignition.
func (e *Engine) ignitionCredential(ctx context.Context, cred *Cred) (string, error) {
	if identityChecked(cred) {
		return e.confirmIdentity(ctx, cred, 0)
	}
	credential, err := e.host.Credential(ctx, hostAccount(cred))
	if err != nil {
		return "", nil
	}
	return credential.Fingerprint, nil
}

// noteSignIn reads the fingerprint of the sign-in of cred, an agy-cli
// account (identityChecked), with each query, as Credential gives that of
// a Codex or Claude account: the quota of such an account comes from
// Magpie, so no query reads its sign-in otherwise. A new sign-in lets
// ignition try again (noteFingerprint). Only a host.IdentityChecker
// reads it, from local files alone; a sign-in that is not confirmed
// changes nothing.
func (e *Engine) noteSignIn(ctx context.Context, cred *Cred) {
	checker, ok := e.host.(host.IdentityChecker)
	if !ok || !identityChecked(cred) {
		return
	}
	if fingerprint, err := checker.ConfirmIdentity(ctx, hostAccount(cred)); err == nil {
		e.noteFingerprint(cred, fingerprint)
	}
}

// sentAfterProblem reports whether a request of cred sent at sent went
// out after the account's credential problem was last found
// (store.CredentialState FailedAt), or the account has none.
func (e *Engine) sentAfterProblem(cred *Cred, sent time.Time) bool {
	if e.state == nil {
		return true
	}
	st := e.state.Credentials[cred.AuthIndex]
	return st == nil || st.Reason == "" || sent.After(st.FailedAt)
}

// noteQueryFailure handles a failed quota query of cred. A suspended account
// is a problem right away, a rejected credential once it lasts; see
// queryProblemAge.
func (e *Engine) noteQueryFailure(ctx context.Context, cred *Cred, fingerprint string, err error) {
	if e.state == nil {
		return
	}
	d := failure.Diagnose(err)
	st := e.state.Credential(cred.AuthIndex)
	now := e.now()
	if d.Reason != failure.ReasonAuth && d.Reason != failure.ReasonSuspended {
		if st.QueryReason != "" {
			st.QueryReason, st.QuerySince, st.QueryCount = "", 0, 0
			e.dirty = true
		}
		e.forgetEmptyCredential(cred.AuthIndex)
		return
	}
	if st.Reason == d.Reason {
		st.FailedAt = now
		e.dirty = true
		return
	}
	if st.QueryReason != d.Reason {
		st.QueryReason, st.QuerySince, st.QueryCount = d.Reason, now.Unix(), 0
	}
	st.QueryCount++
	e.dirty = true
	if d.Reason == failure.ReasonAuth && (st.QueryCount < queryProblemCount || now.Sub(time.Unix(st.QuerySince, 0)) < queryProblemAge) {
		return
	}
	e.noteCredentialProblem(ctx, cred, e.accountGroup(cred.AuthIndex), d, sourceQuery, fingerprint)
}

// noteCredentialProblem records that the provider rejected cred or disabled
// its account, records a credential_invalid event and sends one
// notification. The same problem found again, by ignition or by a query,
// only updates the fingerprint. A problem ignition found or confirmed stays
// ignition-confirmed (Ignited), also when a problem of another reason
// replaces it, until the credential changes; see ignitionConfirmed.
func (e *Engine) noteCredentialProblem(ctx context.Context, cred *Cred, group string, d failure.Diagnosis, source, fingerprint string) {
	st := e.state.Credential(cred.AuthIndex)
	e.dirty = true
	if st.Reason == d.Reason {
		// An unknown fingerprint stays unknown; see noteFingerprint.
		st.Fingerprint = fingerprint
		st.FailedAt = e.now()
		if source == sourceIgnite {
			st.Ignited = true
		}
		st.Changed = false
		return
	}
	ignited := source == sourceIgnite || st.Reason != "" && !st.Changed && ignitionConfirmed(st)
	now := e.now()
	*st = store.CredentialState{
		Reason: d.Reason, Status: d.StatusText(), Code: d.Code, Summary: credentialSummary(d), AppealURL: redact.Text(d.AppealURL),
		Source: source, Group: group, Since: now.Unix(), FailedAt: now, Fingerprint: fingerprint, Ignited: ignited,
		Notice: noticeProblem, NoticeAt: now.Unix(),
	}
	e.publishCredential(cred.AuthIndex, d.Reason)
	e.credentialEvent(cred, group, d, source, time.Time{})
	e.sendCredentialNotice(ctx, cred, st, now)
}

// ignitionConfirmed reports whether an ignition found or confirmed the
// problem of st, which a working query then does not end. State saved
// before Ignited existed has only Source.
func ignitionConfirmed(st *store.CredentialState) bool {
	return st.Ignited || st.Source == sourceIgnite
}

// credentialEvent records a credential_invalid event. until is the end of
// the ignition pause of a rate limit, zero otherwise.
func (e *Engine) credentialEvent(cred *Cred, group string, d failure.Diagnosis, source string, until time.Time) {
	label := accountLabel(cred)
	description := map[string]string{
		failure.ReasonAuth:        "凭证失效",
		failure.ReasonSuspended:   "账号已停用",
		failure.ReasonRateLimited: "被限流",
	}[d.Reason]
	detail := joinParts(" · ", d.StatusText(), d.Code, description)
	if summary := credentialSummary(d); summary != "" {
		detail += "：" + summary
	}
	params := map[string]string{
		"reason": d.Reason, "status": d.StatusText(), "code": d.Code, "summary": credentialSummary(d),
		"appeal_url": d.AppealURL, "source": source,
	}
	if !until.IsZero() {
		params["until"] = eventTime(until)
	}
	e.addAccountEvent(cred.AuthIndex, "error", "credential_invalid", group, label, detail, label+" "+detail, params)
}

// credentialSummary returns the summary of d for credential events and
// notifications, which show the status and code next to it: empty when the
// summary only repeats them.
func credentialSummary(d failure.Diagnosis) string {
	if d.SummaryFallback {
		return ""
	}
	return redact.Text(d.Summary)
}

// noteCredentialOK records that cred worked for source. A working quota
// query ends the problems queries found; it does not show that the
// provider lifted a suspension found by ignition. A successful ignition ends
// every problem. The end is recorded as a credential_restored event and
// notified when the problem was.
func (e *Engine) noteCredentialOK(ctx context.Context, cred *Cred, source string) {
	if e.state == nil {
		return
	}
	st := e.state.Credentials[cred.AuthIndex]
	if st == nil {
		return
	}
	if source == sourceQuery && st.QueryReason != "" {
		st.QueryReason, st.QuerySince, st.QueryCount = "", 0, 0
		e.dirty = true
	}
	if st.Reason == "" || source == sourceQuery && ignitionConfirmed(st) {
		e.forgetEmptyCredential(cred.AuthIndex)
		return
	}
	notified := st.Notified
	now := e.now()
	*st = store.CredentialState{}
	e.dirty = true
	e.publishCredential(cred.AuthIndex, "")
	label := accountLabel(cred)
	detail := map[string]string{sourceIgnite: "点火成功", sourceQuery: "额度查询恢复正常"}[source]
	group := e.accountGroup(cred.AuthIndex)
	e.addAccountEvent(cred.AuthIndex, "info", "credential_restored", group, label, detail,
		label+" 凭证已恢复："+detail, map[string]string{"source": source})
	if !notified {
		e.forgetEmptyCredential(cred.AuthIndex)
		return
	}
	st.Notice, st.NoticeAt, st.Source, st.Group = noticeRestored, now.Unix(), source, group
	e.sendCredentialNotice(ctx, cred, st, now)
}

// forgetEmptyCredential removes the state of an account without problem,
// counter or pending notification.
func (e *Engine) forgetEmptyCredential(authIndex string) {
	if st := e.state.Credentials[authIndex]; st != nil && st.Reason == "" && st.QueryReason == "" && st.Notice == "" {
		delete(e.state.Credentials, authIndex)
		e.dirty = true
	}
}

// sendCredentialNotice sends the pending notification of st. A failed one
// stays pending and is retried with every poll for credentialNoticeAge.
func (e *Engine) sendCredentialNotice(ctx context.Context, cred *Cred, st *store.CredentialState, now time.Time) {
	if st.Notice == "" {
		return
	}
	if now.Sub(time.Unix(st.NoticeAt, 0)) > credentialNoticeAge || e.alerts == nil || e.alerts.Sender == nil {
		st.Notice = ""
		e.dirty = true
		e.forgetEmptyCredential(cred.AuthIndex)
		return
	}
	lang := e.language()
	label := accountLabel(cred)
	var msg notify.Message
	if st.Notice == noticeRestored {
		msg = notify.CredentialOKMessage(lang, label, st.Source == sourceIgnite)
	} else {
		kind := notify.KindCredential
		if st.Reason == failure.ReasonSuspended {
			kind = notify.KindSuspended
		}
		msg = notify.CredentialMessage(lang, label, notify.Problem{
			Kind: kind, Status: st.Status, Code: st.Code, Summary: st.Summary, AppealURL: st.AppealURL,
			Ignition: st.Source == sourceIgnite,
		}, e.config().Location())
	}
	msg.Group = st.Group
	if msg.Group == "" {
		msg.Group = e.accountGroup(cred.AuthIndex)
	}
	if e.credNoticeTried == nil {
		e.credNoticeTried = map[string]time.Time{}
	}
	e.credNoticeTried[cred.AuthIndex] = e.now()
	switch err := e.alerts.Sender.Send(ctx, msg); {
	case errors.Is(err, notify.ErrNotConfigured):
		// Notifications are off; the problem counts as not notified.
	case err != nil:
		e.notifyFailed("", msg, err)
		return
	default:
		e.notified("", msg)
		if st.Notice == noticeProblem {
			st.Notified = true
		}
	}
	st.Notice = ""
	e.dirty = true
	e.forgetEmptyCredential(cred.AuthIndex)
}

// flushCredentialNotices retries the pending credential notifications. A
// notification already sent or tried since start, the start of this poll,
// waits for the next poll: a problem found by a query of this poll tries its
// notification right away, and a second try would only repeat it.
func (e *Engine) flushCredentialNotices(ctx context.Context, start time.Time) {
	if e.state == nil {
		return
	}
	now := e.now()
	for authIndex, st := range e.state.Credentials {
		if st == nil || st.Notice == "" {
			continue
		}
		if tried, ok := e.credNoticeTried[authIndex]; ok && !tried.Before(start) {
			continue
		}
		e.mu.Lock()
		cred := e.creds[authIndex]
		e.mu.Unlock()
		if cred != nil {
			e.sendCredentialNotice(ctx, cred, st, now)
		}
	}
}

func joinParts(sep string, parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, sep)
}

// rateLimitedNotice returns the notification of an ignition paused by a
// rate limit until the next daily start.
func rateLimitedNotice(lang, label string, d failure.Diagnosis, until time.Time, loc *time.Location) notify.Message {
	return notify.CredentialMessage(lang, label, notify.Problem{
		Kind: notify.KindRateLimited, Status: d.StatusText(), Code: d.Code, Summary: credentialSummary(d),
		Ignition: true, Until: until,
	}, loc)
}
