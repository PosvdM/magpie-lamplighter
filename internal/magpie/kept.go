package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
	"github.com/PosvdM/magpie-lamplighter/internal/redact"
)

const (
	// keptPath is Magpie's last readings, read with no vendor asked
	// (provider.KeptQuotaReport).
	keptPath = "/v1/magpie/quotas?kept=1"
	// refreshPath reads cards again (quotaCardsRefresh); the gateway reads
	// one card at most once in 30 seconds and waits at most 30 seconds.
	refreshPath = "/v1/magpie/quotas/refresh"
)

// keptQuota is one card of GET /v1/magpie/quotas (provider.Quota).
type keptQuota struct {
	Provider string       `json:"provider"`
	Kind     string       `json:"kind"`
	User     string       `json:"user"`
	From     string       `json:"from"`
	Windows  []keptWindow `json:"windows"`
	ReadAt   *time.Time   `json:"readAt"`
	// AsOf is set on an earlier reading kept in place of one that failed,
	// with ReadError or ReadDeferred; Error is why the reading failed
	// when none is kept.
	AsOf         *time.Time `json:"asOf"`
	ReadDeferred bool       `json:"readDeferred"`
	ReadError    string     `json:"readError"`
	Error        string     `json:"error"`
}

// keptWindow is one window of a card (provider.QuotaSpan).
type keptWindow struct {
	Name      string     `json:"name"`
	Used      *float64   `json:"used"`
	ResetsAt  *time.Time `json:"resetsAt"`
	Text      bool       `json:"text"`
	Unlimited bool       `json:"unlimited"`
	Lane      string     `json:"lane"`
	Balance   *float64   `json:"balance"`
	// ResetSecs is the reset in seconds, which a card of POST
	// /v1/magpie/quotas/refresh (provider.QuotaWindow) may give instead
	// of resetsAt. GET /v1/magpie/quotas turns it into resetsAt from the
	// time it answers (quotaReport), and keptOf does the same.
	ResetSecs int64 `json:"resetSecs"`
	// Pool is the Antigravity quota pool a window draws on, such as
	// "Gemini"; Family is set on the per-model windows of a pool.
	Pool   string `json:"pool"`
	Family string `json:"family"`
}

// KeptQuotas returns Magpie's last readings of the Codex, Claude and
// agy-cli accounts that are switched on, an agy-cli reading with the plan
// the account list gives the account (planOf). A card that does not decode or convert is
// logged and left out; the other cards are returned. The call, with the
// account list it reads, ends within keptTimeout.
func (h *Host) KeptQuotas(ctx context.Context) ([]host.KeptQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, keptTimeout)
	defer cancel()
	var resp struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodGet, keptPath, &resp); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("GET %s: %w: no data list", routeOf(keptPath), host.ErrFormat)
	}
	listedAt := now()
	listed, err := h.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	on := map[string]listedAccount{}
	for _, a := range listed {
		if a.Kind == "account" && a.On != nil && *a.On {
			on[a.Provider+"\n"+accountKey(a.User)] = a
		}
	}
	cards, bad := decodeCards[keptQuota](*resp.Data)
	h.noteBadCards(listed, bad)
	out := []host.KeptQuota{}
	for _, q := range cards {
		if q.Kind != "subscription" || q.From != "" {
			continue
		}
		a, found := on[q.Provider+"\n"+accountKey(q.User)]
		if !found || strings.TrimSpace(q.User) == "" {
			continue
		}
		k, ok, err := keptOf(q, a.Pin, now())
		h.noteCardError(a.Pin, err)
		if ok && err == nil {
			k.Plan, k.PlanAt = planOf(k, a, listedAt)
			out = append(out, k)
		}
	}
	return out, nil
}

// planOf is the plan of the account list to give with k, the reading of
// a, and when the list was begun to be read, listedAt: only an agy-cli
// reading carries them (host.KeptQuota Plan and PlanAt).
func planOf(k host.KeptQuota, a listedAccount, listedAt time.Time) (string, time.Time) {
	plan := strings.TrimSpace(a.Plan)
	if !k.Active || plan == "" {
		return "", time.Time{}
	}
	return plan, listedAt
}

// cardID is what tells whose a card is, read from a card that may not
// decode as a whole.
type cardID struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	User     string `json:"user"`
	From     string `json:"from"`
}

// badCard is a card that does not decode: its index in the list, whose it
// is when that can be read, and the error.
type badCard struct {
	cardID
	index int
	err   error
}

// name names b in the log when it belongs to no account switched on: by
// provider and masked account name, else by its place in the list.
func (b badCard) name() string {
	if b.Provider != "" && b.User != "" {
		return b.Provider + " " + maskEmail(b.User)
	}
	return fmt.Sprintf("card %d", b.index+1)
}

// decodeCards decodes each card of raw on its own, so that a card of
// another shape, such as one with a readAt that is no time or a used
// given as text, costs only itself. The cards that decode are returned
// in their order; each of the others is a badCard, its error wrapping
// host.ErrFormat.
func decodeCards[T any](raw []json.RawMessage) ([]T, []badCard) {
	out := make([]T, 0, len(raw))
	var bad []badCard
	for i, r := range raw {
		var card T
		if err := json.Unmarshal(r, &card); err != nil {
			b := badCard{index: i, err: fmt.Errorf("%w: %v", host.ErrFormat, err)}
			_ = json.Unmarshal(r, &b.cardID)
			bad = append(bad, b)
			continue
		}
		out = append(out, card)
	}
	return out, bad
}

// noteBadCards logs each of bad, cards that do not decode, as
// noteCardError does: by the pin of the listed account it belongs to, else
// by its name.
func (h *Host) noteBadCards(listed []listedAccount, bad []badCard) {
	for _, b := range bad {
		name := b.name()
		for _, a := range listed {
			if b.isCardOf(a) {
				name = a.Pin
				break
			}
		}
		h.noteCardError(name, b.err)
	}
}

// ownBadCard is the card of a among bad, nil for none.
func ownBadCard(bad []badCard, a listedAccount) *badCard {
	for i := range bad {
		if bad[i].isCardOf(a) {
			return &bad[i]
		}
	}
	return nil
}

// isCardOf reports whether the card b would be the subscription card of a
// (cardIndex).
func (b badCard) isCardOf(a listedAccount) bool {
	return cardIndex([]cardState{{Provider: b.Provider, Kind: b.Kind, User: b.User, From: b.From}}, a) == 0
}

// noteCardError logs err, the error of converting the kept card of the
// account pin, when it differs from the last one logged for the account:
// KeptQuotas runs every minute, and a card keeps its format until Magpie
// reads the account again. A nil err forgets the last one.
func (h *Host) noteCardError(pin string, err error) {
	text := ""
	if err != nil {
		text = fmt.Sprintf("GET %s: %s: %v", routeOf(keptPath), pin, err)
	}
	h.mu.Lock()
	last := h.cardErrors[pin]
	if text == "" {
		delete(h.cardErrors, pin)
	} else {
		if h.cardErrors == nil {
			h.cardErrors = map[string]string{}
		}
		h.cardErrors[pin] = text
	}
	h.mu.Unlock()
	if text != "" && text != last {
		h.Log("warn", "跳过 Magpie 额度记录中格式不对的卡："+text, nil)
	}
}

// keptOf converts the card q of the account pin into a KeptQuota; ok is
// false for a card without a reading time or windows Lamplighter reads,
// and for an agy-cli card that is no new reading of Magpie's own
// (agyFresh). A window that tells its reset only in seconds (ResetSecs)
// is placed from at, the time of the answer.
func keptOf(q keptQuota, pin string, at time.Time) (k host.KeptQuota, ok bool, err error) {
	if q.Provider == agyCLI && !agyFresh(q) {
		return host.KeptQuota{}, false, nil
	}
	readAt := q.ReadAt
	if readAt == nil {
		readAt = q.AsOf
	}
	if readAt == nil || readAt.IsZero() {
		return host.KeptQuota{}, false, nil
	}
	ws := make([]keptWindow, len(q.Windows))
	for i, w := range q.Windows {
		if w.ResetsAt == nil && w.ResetSecs > 0 {
			reset := at.Add(time.Duration(w.ResetSecs) * time.Second)
			w.ResetsAt = &reset
		}
		ws[i] = w
	}
	var groups []quota.RawGroup
	var unread []string
	switch q.Provider {
	case "codex":
		groups, err = keptCodex(ws)
	case "claude":
		groups, err = keptClaude(ws)
	case agyCLI:
		groups, unread, err = keptAgy(ws)
	default:
		return host.KeptQuota{}, false, nil
	}
	if err != nil || len(groups) == 0 {
		return host.KeptQuota{}, false, err
	}
	k = host.KeptQuota{Pin: pin, Groups: groups, ReadAt: readAt.UTC()}
	if q.Provider == agyCLI {
		k.Active, k.Unread = true, unread
	}
	return k, true, nil
}

// agyFresh reports whether the agy-cli card q is a reading Magpie made
// this time: it has readAt, and no error, readError, readDeferred or
// asOf. When agy reads nothing, such as while its sign-in waits to be
// renewed, the card has only error; when a reading fails or is put off,
// Magpie shows the last good one with asOf and readError or readDeferred
// (lastQuotas in internal/provider/quota_last.go), and may give a window
// whose reset has passed since as unused and without a reset. Such a card
// tells nothing of the account as it is now: neither 0% nor 100%.
func agyFresh(q keptQuota) bool {
	return q.Error == "" && q.ReadError == "" && !q.ReadDeferred && q.AsOf == nil && q.ReadAt != nil && !q.ReadAt.IsZero()
}

// counted is whether w is a reading of a share used: not a text row, an
// unlimited allowance, another lane or a wallet, and not the 5-hour
// window Magpie shows for a Claude account that has not started one (no
// reset, nothing used), which the usage API leaves out.
func counted(w keptWindow) (bool, error) {
	if w.Text || w.Unlimited || w.Lane != "" || w.Balance != nil {
		return false, nil
	}
	if w.Used == nil {
		return false, fmt.Errorf("%w: %s has no used", host.ErrFormat, windowName(w.Name))
	}
	return w.ResetsAt != nil || *w.Used != 0, nil
}

// windowName names the window called name in an error: by its name when
// it is one Lamplighter converts, otherwise as "a window", since the name
// comes from Magpie's card and may be any text.
func windowName(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if _, known := claudeKept[key]; known || codexSpan.MatchString(key) {
		return fmt.Sprintf("window %q", key)
	}
	return "a window"
}

func keptWindowOf(w keptWindow, id, label string, period time.Duration) quota.Window {
	out := quota.Window{ID: id, Label: label, Remaining: remaining(*w.Used), Period: period}
	if w.ResetsAt != nil {
		out.Reset = w.ResetsAt.UTC()
	}
	return out
}

// remaining is 100 minus used, kept within 0–100 and rounded as the
// providers round theirs.
func remaining(used float64) float64 {
	v := math.Round((100-used)*1e6) / 1e6
	return min(max(v, 0), 100)
}

var codexSpan = regexp.MustCompile(`^(\d+) (hours|days)$`)

// keptCodex converts a Codex card into the group of the Codex Fetch:
// "codex:main" with the 5-hour and 7-day windows, and other lengths as
// "window-<length>". Magpie names a window by its length
// (quotaDurationName): "5 hours", "7 days", "30 days"; "Allowance" has
// no length and is left out.
func keptCodex(ws []keptWindow) ([]quota.RawGroup, error) {
	group := quota.RawGroup{Key: "codex:main", SourceLabel: providers.Title("codex")}
	for _, w := range ws {
		ok, err := counted(w)
		if err != nil {
			return nil, err
		}
		m := codexSpan.FindStringSubmatch(strings.ToLower(strings.TrimSpace(w.Name)))
		if !ok || m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		period := time.Duration(n) * time.Hour
		if m[2] == "days" {
			period *= 24
		}
		id, label, standard := quota.Standard(period)
		if !standard {
			label = quota.PeriodLabel(period)
			id = "window-" + label
		}
		group.Windows = append(group.Windows, keptWindowOf(w, id, label, period))
	}
	if len(group.Windows) == 0 {
		return nil, nil
	}
	return []quota.RawGroup{group}, nil
}

// claudeKept maps the window names Magpie gives a Claude account
// (claudeKinds in internal/provider/claude_limits.go) to the groups and
// windows of the Claude Fetch.
var claudeKept = map[string]struct {
	group, source, id, label string
	period                   time.Duration
}{
	"5 hours":         {"claude:main", "Claude", quota.WindowFiveHour, quota.LabelFiveHour, quota.FiveHours},
	"7 days":          {"claude:main", "Claude", quota.WindowSevenDay, quota.LabelSevenDay, quota.SevenDays},
	"7 days · opus":   {"claude:seven-day-opus", "Claude", "seven-day-opus", "7 Day Opus", quota.SevenDays},
	"7 days · sonnet": {"claude:seven-day-sonnet", "Claude", "seven-day-sonnet", "7 Day Sonnet", quota.SevenDays},
	"7 days · fable":  {providers.FableGroupKey, "Fable", quota.WindowFable, quota.LabelFable, quota.SevenDays},
}

// keptClaude converts a Claude card into the groups of the Claude Fetch.
// Windows of other names, such as "Extra usage", are left out.
func keptClaude(ws []keptWindow) ([]quota.RawGroup, error) {
	var groups []quota.RawGroup
	index := map[string]int{}
	for _, w := range ws {
		ok, err := counted(w)
		if err != nil {
			return nil, err
		}
		spec, known := claudeKept[strings.ToLower(strings.TrimSpace(w.Name))]
		if !ok || !known {
			continue
		}
		i, seen := index[spec.group]
		if !seen {
			i = len(groups)
			index[spec.group] = i
			groups = append(groups, quota.RawGroup{Key: spec.group, SourceLabel: spec.source})
		}
		groups[i].Windows = append(groups[i].Windows, keptWindowOf(w, spec.id, spec.label, spec.period))
	}
	return groups, nil
}

// agyCLI is the provider id of the agy-cli plugin
// (@magpie-community/opencode-agy-cli-auth), which serves Antigravity.
const agyCLI = "agy-cli"

// agyKept maps the length in the name of a pool's own window to the
// window: "5 hours" and "7 days" (the plugin's cadence in usage.mjs).
var agyKept = map[string]struct {
	label  string
	period time.Duration
	weekly bool
}{
	"5 hours": {quota.LabelFiveHour, quota.FiveHours, false},
	"7 days":  {quota.LabelSevenDay, quota.SevenDays, true},
}

// keptAgy converts an agy-cli card into one group per quota pool
// (providers.AntigravityPool), with the pool's 5-hour and 7-day windows.
//
// The plugin reads agy's /usage (parseUsageReport in its usage.mjs): each
// pool has windows of its own, {pool: "Gemini", name: "5 hours"}, and a
// window per model family drawing on it, named after the family and with
// family set. GET ?kept=1 puts the pool's name before its own windows'
// ("Gemini · 5 hours", Magpie's PooledWindows) and leaves out the model
// windows; the refresh answers the plugin's names as they are. Only the
// pool's own 5-hour and 7-day windows are read.
//
// A pool with a window that has no figure ("Limits not available", which
// Magpie sends as a text row) is not read: its key is returned in unread
// and none of its windows is, so that its group stays as it was and is not
// taken for one that is gone. A pool none of whose own windows is read
// (all text rows, or none listed) is left out.
func keptAgy(ws []keptWindow) (groups []quota.RawGroup, unread []string, err error) {
	type pool struct {
		group  quota.RawGroup
		unread bool
	}
	var pools []*pool
	index := map[string]*pool{}
	for _, w := range ws {
		name := strings.TrimSpace(w.Pool)
		if name == "" || w.Family != "" {
			continue
		}
		window := strings.TrimSpace(w.Name)
		if before, after, ok := strings.Cut(window, " · "); ok && strings.EqualFold(strings.TrimSpace(before), name) {
			window = strings.TrimSpace(after)
		}
		spec, known := agyKept[strings.ToLower(window)]
		g, named := providers.AntigravityPool(name)
		if !known || !named {
			continue
		}
		p := index[g.Key]
		if p == nil {
			p = &pool{group: quota.RawGroup{Key: g.Key, SourceLabel: g.SourceLabel}}
			index[g.Key] = p
			pools = append(pools, p)
		}
		if w.Text || w.Unlimited || w.Lane != "" || w.Balance != nil {
			p.unread = true
			continue
		}
		if w.Used == nil {
			return nil, nil, fmt.Errorf("%w: a pool window has no used", host.ErrFormat)
		}
		id := g.FiveHour
		if spec.weekly {
			id = g.Weekly
		}
		if slices.ContainsFunc(p.group.Windows, func(x quota.Window) bool { return x.ID == id }) {
			return nil, nil, fmt.Errorf("%w: pool window %q appears twice", host.ErrFormat, window)
		}
		p.group.Windows = append(p.group.Windows, keptWindowOf(w, id, spec.label, spec.period))
	}
	for _, p := range pools {
		switch {
		case p.unread:
			unread = append(unread, p.group.Key)
		case len(p.group.Windows) > 0:
			slices.SortStableFunc(p.group.Windows, func(a, b quota.Window) int { return int(a.Period - b.Period) })
			groups = append(groups, p.group)
		}
	}
	return groups, unread, nil
}

// Minimum time between two quota refreshes of one account. A Claude
// refresh runs Claude Code's /usage, and an agy-cli refresh agy's /usage,
// which sends Google a dozen Code Assist requests; the others ask the
// vendor once.
const (
	agyRefreshGap    = 15 * time.Minute
	claudeRefreshGap = 10 * time.Minute
	refreshGap       = time.Minute
)

// The host.Error codes of RefreshQuota besides those of gatewayJSON.
const (
	// codeAccountOff: the account is switched off in Magpie.
	codeAccountOff = "account_off"
	// codeAccountNotFound: Magpie has no subscription account with the
	// pin, or does not name it.
	codeAccountNotFound = "account_not_found"
	// codeGatewayUnavailable: the gateway cannot be reached, answers 5xx
	// or does not answer within refreshTimeout.
	codeGatewayUnavailable = "gateway_unavailable"
	// codeRefreshStale: the gateway answered, but its card of the account
	// is no new reading (refreshStale). Magpie's kept reading still
	// stands.
	codeRefreshStale = "refresh_stale"
)

// cardState is what a card of GET /v1/magpie/quotas?kept=1
// (provider.Quota) or of POST /v1/magpie/quotas/refresh
// (provider.SubscriptionQuota, as writeCards answers) tells of its
// reading. Both are made from provider.CachedCards, so their readAt can be
// compared.
type cardState struct {
	Provider string     `json:"provider"`
	Kind     string     `json:"kind"`
	User     string     `json:"user"`
	From     string     `json:"from"`
	ReadAt   *time.Time `json:"readAt"`
	// AsOf is when a reading kept in place of one that failed was read.
	AsOf *time.Time `json:"asOf"`
	// ReadDeferred is a reading Magpie chose not to make, such as for a
	// provider read only when the user asks; ReadError is why the last
	// reading failed while an earlier one is shown; Error is why it
	// failed when none is.
	ReadDeferred bool   `json:"readDeferred"`
	ReadError    string `json:"readError"`
	Error        string `json:"error"`
}

// at is when the card's reading was made, nil when not known.
func (c cardState) at() *time.Time {
	if c.ReadAt != nil && !c.ReadAt.IsZero() {
		return c.ReadAt
	}
	if c.AsOf != nil && !c.AsOf.IsZero() {
		return c.AsOf
	}
	return nil
}

// cardIndex is the index of the subscription card of a among cards, -1
// for none: this Magpie's own, not a remote one, matched by provider and
// account name as KeptQuotas matches them.
func cardIndex(cards []cardState, a listedAccount) int {
	for i, c := range cards {
		if c.From == "" && (c.Kind == "" || c.Kind == "subscription") && c.Provider == a.Provider && accountKey(c.User) == accountKey(a.User) {
			return i
		}
	}
	return -1
}

// refreshStale is why card, as the refresh answered it, is no new
// reading since before, the time of the reading the card had right
// before the refresh (nil for none); "" when it is one. The gateway
// answers the cards it has even when it read none again: within its 30
// seconds for a card, when the reading failed, or when Magpie put it off.
func refreshStale(card cardState, found bool, before *time.Time) string {
	switch {
	case !found:
		return "Magpie 的回复中没有这个账号的额度"
	case card.ReadDeferred:
		return "Magpie 按设置暂不读取这个账号的额度"
	case card.ReadError != "":
		return "读取失败：" + shortReason(card.ReadError)
	case card.Error != "":
		return "读取失败：" + shortReason(card.Error)
	}
	after := card.at()
	switch {
	case after == nil:
		return "Magpie 没有给出读取时间"
	case before != nil && !after.After(*before):
		return "Magpie 返回的仍是 " + after.UTC().Format("2006-01-02 15:04:05 UTC") + " 的读数"
	}
	return ""
}

// maskEmail keeps the last two characters of the local part of email,
// such as "***rk", as the MCP tools name an account (redact.Email).
func maskEmail(email string) string { return redact.Email(email) }

// Reasons Magpie gives are kept to reasonLimit characters, with emails
// shown as maskEmail shows them and long token-like strings left out.
const reasonLimit = 120

var (
	reasonEmail  = regexp.MustCompile(`[^\s@"'<>()\[\],;:]+@[^\s@"'<>()\[\],;:]+`)
	reasonSecret = regexp.MustCompile(`[A-Za-z0-9_\-./+=]{32,}`)
)

// shortReason is a reason Magpie gave, fit to show: one line, without
// emails or secrets, at most reasonLimit characters.
func shortReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = reasonEmail.ReplaceAllStringFunc(s, maskEmail)
	s = reasonSecret.ReplaceAllString(s, "…")
	if r := []rune(s); len(r) > reasonLimit {
		s = string(r[:reasonLimit]) + "…"
	}
	return s
}

// ErrRefreshLimited is returned when an account's quota was refreshed too
// recently; see agyRefreshGap.
var ErrRefreshLimited = host.ErrRefreshLimited

// RefreshQuota asks Magpie to read the quota of the account pin again;
// see RefreshKept.
func (h *Host) RefreshQuota(caller context.Context, pin string) error {
	_, _, err := h.RefreshKept(caller, pin)
	return err
}

// RefreshKept asks Magpie to read the quota of the account pin again, and
// returns the account's card of the answer as a KeptQuota (keptOf), an
// agy-cli card with the plan of the account list (planOf), found when
// there is one to use; it implements host.KeptRefresher. The cards of the answer are decoded one
// by one (decodeCards): a card of another account that does not decode is
// logged and left out.
//
// Right before, it reads the account list again: the account must still
// be a subscription account that is switched on and has a name, since the
// gateway refreshes every account of the provider when no name is given.
// Those failures, and a gateway that cannot be reached, answers 5xx or
// runs out of time, are a *host.Error with the codes above.
//
// The time of the account's kept reading (GET ?kept=1) is read before the
// refresh. When the card the refresh answers is no newer reading
// (refreshStale), it returns a *host.Error with codeRefreshStale, and the
// card when it converts. A card of the account that does not decode or
// convert, and a newer card with no window Lamplighter reads, give an
// error that wraps host.ErrFormat. A newer card that does not convert, or
// the account's card that does not decode, is settled by keptAfter: that
// error, unless the kept quota has a newer reading that converts, which is
// returned marked Reread. So a call without an error always returns a card
// newer than the reading before the refresh; only one not marked Reread is
// the refresh's own card, which alone confirms an ignition.
//
// The account list and the kept reading end within refreshLookTimeout,
// and the refresh itself within refreshTimeout, so that a slow reading
// before it does not shorten the refresh. Both also end with caller.
//
// It refuses within agyRefreshGap (agy-cli), claudeRefreshGap (Claude) or
// refreshGap (others) of the last refresh sent for the same account,
// whatever its result. A call that fails or whose caller has ended before
// the refresh is sent does not count. Right before it sends the refresh,
// it calls host.RefreshSending with the time the gap counts from; an
// error of it stops the refresh unsent, and does not count either. The refresh of an agy-cli account
// that confirms an ignition (host.IsConfirmingIgnition) is not held by
// the gap, and still starts it; the engine sends at most three of them
// per ignition.
func (h *Host) RefreshKept(caller context.Context, pin string) (host.KeptQuota, bool, error) {
	look, cancelLook := context.WithTimeout(caller, refreshLookTimeout)
	defer cancelLook()
	route := http.MethodPost + " " + refreshPath
	listedAt := now()
	listed, err := h.listAccounts(look)
	if err != nil {
		return host.KeptQuota{}, false, refreshError(caller, route, err)
	}
	var a listedAccount
	found := false
	for _, l := range listed {
		if strings.EqualFold(l.Pin, strings.TrimSpace(pin)) {
			a, found = l, true
			break
		}
	}
	switch {
	case !found:
		return host.KeptQuota{}, false, &host.Error{Method: route, Code: codeAccountNotFound, Message: "Magpie 中没有这个账号"}
	case a.Kind != "account":
		return host.KeptQuota{}, false, &host.Error{Method: route, Code: codeAccountNotFound, Message: "这个账号不是订阅账号"}
	case strings.TrimSpace(a.User) == "":
		return host.KeptQuota{}, false, &host.Error{Method: route, Code: codeAccountNotFound, Message: "Magpie 没有给出这个账号的名称"}
	case !*a.On:
		return host.KeptQuota{}, false, &host.Error{Method: route, Code: codeAccountOff, Message: "这个账号已在 Magpie 中停用"}
	}
	confirming := host.IsConfirmingIgnition(caller)
	if _, _, err := h.refreshAllowed(a, false, confirming); err != nil {
		return host.KeptQuota{}, false, err
	}
	var kept struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := h.gatewayJSON(look, http.MethodGet, keptPath, &kept); err != nil {
		return host.KeptQuota{}, false, refreshError(caller, route, err)
	}
	if kept.Data == nil {
		return host.KeptQuota{}, false, fmt.Errorf("GET %s: %w: no data list", routeOf(keptPath), host.ErrFormat)
	}
	// A card of the account that does not decode leaves the time of its
	// reading unknown, as no card does.
	keptCards, bad := decodeCards[cardState](*kept.Data)
	h.noteBadCards(listed, bad)
	var before *time.Time
	if i := cardIndex(keptCards, a); i >= 0 {
		before = keptCards[i].at()
	}
	cancelLook()
	// A caller that has ended sends no refresh, which would start the
	// account's gap for nothing.
	if caller.Err() != nil {
		return host.KeptQuota{}, false, fmt.Errorf("%s: %w", route, ended(caller))
	}
	// Checked again: another call may have sent a refresh meanwhile.
	at, last, err := h.refreshAllowed(a, true, confirming)
	if err != nil {
		return host.KeptQuota{}, false, err
	}
	if err := host.RefreshSending(caller, at); err != nil {
		// The caller stopped the refresh: it was not sent.
		h.unsend(a.Pin, at, last)
		return host.KeptQuota{}, false, err
	}
	ctx, cancel := context.WithTimeout(caller, refreshTimeout)
	defer cancel()
	query := url.Values{"provider": {a.Provider}, "user": {a.User}}
	var resp struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodPost, refreshPath+"?"+query.Encode(), &resp); err != nil {
		return host.KeptQuota{}, false, refreshError(caller, route, err)
	}
	if resp.Data == nil {
		return host.KeptQuota{}, false, fmt.Errorf("%s: %w: no data list", route, host.ErrFormat)
	}
	answered := now()
	cards, bad := decodeCards[refreshedCard](*resp.Data)
	h.noteBadCards(listed, bad)
	states := make([]cardState, len(cards))
	for i, c := range cards {
		states[i] = c.cardState
	}
	var card cardState
	var k host.KeptQuota
	usable := false
	var convErr error
	i := cardIndex(states, a)
	if own := ownBadCard(bad, a); i < 0 && own != nil {
		// The account's card is there, but does not decode.
		return h.keptAfter(ctx, a, before, listedAt, fmt.Errorf("%s: %s: %w", route, a.Pin, own.err))
	}
	if i >= 0 {
		card = states[i]
		k, usable, convErr = cards[i].kept(a.Pin, answered)
		k.Plan, k.PlanAt = planOf(k, a, listedAt)
	}
	stale := refreshStale(card, i >= 0, before)
	switch {
	case convErr != nil && stale == "":
		return h.keptAfter(ctx, a, before, listedAt, fmt.Errorf("%s: %s: %w", route, a.Pin, convErr))
	case convErr != nil:
		// The card is no new reading and does not convert either: the
		// format is wrong, whatever the refresh did.
		return host.KeptQuota{}, false, fmt.Errorf("%s: %s: %w", route, a.Pin, convErr)
	case stale != "":
		return k, usable, &host.Error{Method: route, Code: codeRefreshStale, Message: stale}
	case !usable:
		// Magpie read the account again, but its card has no window
		// Lamplighter reads, such as a Claude account none of whose
		// windows has started. Its kept quota has
		// no newer reading either, so the refresh brought none to apply.
		return host.KeptQuota{}, false, fmt.Errorf("%s: %s: %w: %s", route, a.Pin, host.ErrFormat, errNoWindows)
	}
	return k, true, nil
}

// errNoWindows is why a new card that converts to no KeptQuota is no
// refresh: it has no window Lamplighter reads.
const errNoWindows = "the new reading has no window Lamplighter reads"

// contextEnded is the error of a context that has ended: its text is the
// context's cause, and it wraps both the cause and the context's own
// error, so that errors.Is tells context.DeadlineExceeded or
// context.Canceled also for a context that ended with a cause of its own.
type contextEnded struct{ err, cause error }

func (c contextEnded) Error() string { return c.cause.Error() }

func (c contextEnded) Unwrap() []error {
	if c.cause == c.err {
		return []error{c.err}
	}
	return []error{c.err, c.cause}
}

// ended is the error of ctx, which has ended.
func ended(ctx context.Context) error {
	return contextEnded{err: ctx.Err(), cause: context.Cause(ctx)}
}

// keptAfter reads Magpie's kept quota again after a refresh whose card
// of the account a is a new reading that does not convert or decode
// (failed, which wraps host.ErrFormat). The kept card counts only when it
// is a new reading as the refresh would be (refreshStale against before,
// the time of the account's reading before the refresh) and converts;
// then it is returned as the refresh's card, marked Reread: the refresh
// did not answer it, so it may show the quota and count as a refresh that
// worked, but confirms no ignition. Otherwise, the kept card being the old
// reading or none, it returns failed: neither success nor a stale refresh
// can be told. listedAt is when the account list of a was begun to be read.
func (h *Host) keptAfter(ctx context.Context, a listedAccount, before *time.Time, listedAt time.Time, failed error) (host.KeptQuota, bool, error) {
	var kept struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodGet, keptPath, &kept); err != nil || kept.Data == nil {
		return host.KeptQuota{}, false, failed
	}
	cards, _ := decodeCards[refreshedCard](*kept.Data)
	states := make([]cardState, len(cards))
	for i, c := range cards {
		states[i] = c.cardState
	}
	i := cardIndex(states, a)
	if i < 0 || refreshStale(states[i], true, before) != "" {
		return host.KeptQuota{}, false, failed
	}
	k, usable, err := cards[i].kept(a.Pin, now())
	if err != nil || !usable {
		return host.KeptQuota{}, false, failed
	}
	k.Plan, k.PlanAt = planOf(k, a, listedAt)
	k.Reread = true
	return k, true, nil
}

// refreshedCard is a card of POST /v1/magpie/quotas/refresh, or of GET
// /v1/magpie/quotas?kept=1: its reading state and its windows.
type refreshedCard struct {
	cardState
	Windows []keptWindow `json:"windows"`
}

// kept converts c, the card of the account pin, as keptOf does with at
// the time of the answer.
func (c refreshedCard) kept(pin string, at time.Time) (host.KeptQuota, bool, error) {
	return keptOf(keptQuota{Provider: c.Provider, Kind: "subscription", User: c.User, Windows: c.Windows, ReadAt: c.ReadAt,
		AsOf: c.AsOf, ReadDeferred: c.ReadDeferred, ReadError: c.ReadError, Error: c.Error}, pin, at)
}

// refreshAllowed returns ErrRefreshLimited when a refresh of a was sent
// within its gap (agyRefreshGap, claudeRefreshGap or refreshGap), unless
// confirming lets an agy-cli refresh through. With send, an allowed
// refresh is recorded as sent at at, now, and last is the time recorded
// before, zero for none.
func (h *Host) refreshAllowed(a listedAccount, send, confirming bool) (at, last time.Time, err error) {
	gap := refreshGap
	switch a.Provider {
	case "claude":
		gap = claudeRefreshGap
	case agyCLI:
		gap = agyRefreshGap
		if confirming {
			gap = 0
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	last = h.refreshed[a.Pin]
	at = now()
	if !last.IsZero() && at.Sub(last) < gap {
		return time.Time{}, last, fmt.Errorf("%w：%s 在 %s 后才能再次刷新", ErrRefreshLimited, a.Pin, (gap - at.Sub(last)).Round(time.Second))
	}
	if !send {
		return at, last, nil
	}
	if h.refreshed == nil {
		h.refreshed = map[string]time.Time{}
	}
	h.refreshed[a.Pin] = at
	return at, last, nil
}

// unsend forgets the refresh of the account pin recorded as sent at at,
// which was not sent after all: the time recorded before, last, stands
// again.
func (h *Host) unsend(pin string, at, last time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.refreshed[pin].Equal(at) {
		return
	}
	if last.IsZero() {
		delete(h.refreshed, pin)
	} else {
		h.refreshed[pin] = last
	}
}

// refreshError is err of a RefreshQuota request, as codeGatewayUnavailable
// when the gateway cannot be reached, answers 5xx or runs out of
// refreshLookTimeout or refreshTimeout. Other errors, such as a 4xx answer
// or one of an unknown format, and any error after the caller ended
// (cancelled, or its own deadline passed), are returned as they are.
func refreshError(caller context.Context, route string, err error) error {
	var hostErr *host.Error
	switch {
	case caller.Err() != nil:
		return err
	case errors.As(err, &hostErr) && (hostErr.Code == "gateway_unreachable" || hostErr.Code == "gateway_http_error" && hostErr.Status >= 500):
		return &host.Error{Method: route, Code: codeGatewayUnavailable, Message: "Magpie 网关暂时不可用", Status: hostErr.Status}
	case errors.Is(err, context.DeadlineExceeded):
		return &host.Error{Method: route, Code: codeGatewayUnavailable, Message: "Magpie 网关没有按时响应"}
	}
	return err
}
