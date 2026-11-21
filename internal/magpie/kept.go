package magpie

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
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
	AsOf     *time.Time   `json:"asOf"`
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
}

// KeptQuotas returns Magpie's last readings of the Codex and Claude
// accounts. Antigravity is left out: Magpie names its windows by pool
// ("Gemini · 5 hours") without the bucket ids that the Antigravity Fetch
// uses as window IDs, so its readings would update no window. The call,
// with the account list it reads, ends within keptTimeout.
func (h *Host) KeptQuotas(ctx context.Context) ([]host.KeptQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, keptTimeout)
	defer cancel()
	var resp struct {
		Data *[]keptQuota `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodGet, keptPath, &resp); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("GET %s: %w: no data list", routeOf(keptPath), host.ErrFormat)
	}
	listed, err := h.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	pins := map[string]string{}
	for _, a := range listed {
		if a.Kind == "account" {
			pins[a.Provider+"\n"+accountKey(a.User)] = a.Pin
		}
	}
	out := []host.KeptQuota{}
	for _, q := range *resp.Data {
		if q.Kind != "subscription" || q.From != "" {
			continue
		}
		pin := pins[q.Provider+"\n"+accountKey(q.User)]
		if pin == "" || strings.TrimSpace(q.User) == "" {
			continue
		}
		readAt := q.ReadAt
		if readAt == nil {
			readAt = q.AsOf
		}
		if readAt == nil || readAt.IsZero() {
			continue
		}
		var groups []quota.RawGroup
		switch q.Provider {
		case "codex":
			groups, err = keptCodex(q.Windows)
		case "claude":
			groups, err = keptClaude(q.Windows)
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("GET %s: %s: %w", routeOf(keptPath), pin, err)
		}
		if len(groups) > 0 {
			out = append(out, host.KeptQuota{Pin: pin, Groups: groups, ReadAt: readAt.UTC()})
		}
	}
	return out, nil
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
		return false, fmt.Errorf("%w: window %q has no used", host.ErrFormat, w.Name)
	}
	return w.ResetsAt != nil || *w.Used != 0, nil
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

// Minimum time between two quota refreshes of one account. A Claude
// refresh runs Claude Code's /usage; the others ask the vendor once.
const (
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

// cardOf is the subscription card of a among cards: this Magpie's own,
// not a remote one, matched by provider and account name as KeptQuotas
// matches them.
func cardOf(cards []cardState, a listedAccount) (cardState, bool) {
	for _, c := range cards {
		if c.From == "" && (c.Kind == "" || c.Kind == "subscription") && c.Provider == a.Provider && accountKey(c.User) == accountKey(a.User) {
			return c, true
		}
	}
	return cardState{}, false
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
// recently; see claudeRefreshGap.
var ErrRefreshLimited = errors.New("额度刷新过于频繁")

// RefreshQuota asks Magpie to read the quota of the account pin again.
// Right before, it reads the account list again: the account must still
// be a subscription account that is switched on and has a name, since the
// gateway refreshes every account of the provider when no name is given.
// Those failures, and a gateway that cannot be reached, answers 5xx or
// runs out of refreshTimeout, are a *host.Error with the codes above.
//
// The time of the account's kept reading (GET ?kept=1) is read before the
// refresh. When the card the refresh answers is no newer reading
// (refreshStale), it returns a *host.Error with codeRefreshStale.
//
// It refuses within claudeRefreshGap (Claude) or refreshGap (others) of
// the last attempt for the same account, whatever its result. The whole
// call ends within refreshTimeout.
func (h *Host) RefreshQuota(caller context.Context, pin string) error {
	ctx, cancel := context.WithTimeout(caller, refreshTimeout)
	defer cancel()
	route := http.MethodPost + " " + refreshPath
	listed, err := h.listAccounts(ctx)
	if err != nil {
		return refreshError(caller, route, err)
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
		return &host.Error{Method: route, Code: codeAccountNotFound, Message: "Magpie 中没有这个账号"}
	case a.Kind != "account":
		return &host.Error{Method: route, Code: codeAccountNotFound, Message: "这个账号不是订阅账号"}
	case strings.TrimSpace(a.User) == "":
		return &host.Error{Method: route, Code: codeAccountNotFound, Message: "Magpie 没有给出这个账号的名称"}
	case !*a.On:
		return &host.Error{Method: route, Code: codeAccountOff, Message: "这个账号已在 Magpie 中停用"}
	}
	gap := refreshGap
	if a.Provider == "claude" {
		gap = claudeRefreshGap
	}
	h.mu.Lock()
	last := h.refreshed[a.Pin]
	if t := now(); !last.IsZero() && t.Sub(last) < gap {
		h.mu.Unlock()
		return fmt.Errorf("%w：%s 在 %s 后才能再次刷新", ErrRefreshLimited, a.Pin, (gap - t.Sub(last)).Round(time.Second))
	}
	if h.refreshed == nil {
		h.refreshed = map[string]time.Time{}
	}
	h.refreshed[a.Pin] = now()
	h.mu.Unlock()
	var kept struct {
		Data *[]cardState `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodGet, keptPath, &kept); err != nil {
		return refreshError(caller, route, err)
	}
	if kept.Data == nil {
		return fmt.Errorf("GET %s: %w: no data list", routeOf(keptPath), host.ErrFormat)
	}
	var before *time.Time
	if card, ok := cardOf(*kept.Data, a); ok {
		before = card.at()
	}
	query := url.Values{"provider": {a.Provider}, "user": {a.User}}
	var resp struct {
		Data *[]cardState `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodPost, refreshPath+"?"+query.Encode(), &resp); err != nil {
		return refreshError(caller, route, err)
	}
	if resp.Data == nil {
		return fmt.Errorf("%s: %w: no data list", route, host.ErrFormat)
	}
	card, found := cardOf(*resp.Data, a)
	if reason := refreshStale(card, found, before); reason != "" {
		return &host.Error{Method: route, Code: codeRefreshStale, Message: reason}
	}
	return nil
}

// refreshError is err of a RefreshQuota request, as codeGatewayUnavailable
// when the gateway cannot be reached, answers 5xx or runs out of time.
// Other errors, such as a 4xx answer or one of an unknown format, and any
// error after the caller cancelled, are returned as they are.
func refreshError(caller context.Context, route string, err error) error {
	var hostErr *host.Error
	switch {
	case errors.Is(caller.Err(), context.Canceled):
		return err
	case errors.As(err, &hostErr) && (hostErr.Code == "gateway_unreachable" || hostErr.Code == "gateway_http_error" && hostErr.Status >= 500):
		return &host.Error{Method: route, Code: codeGatewayUnavailable, Message: "Magpie 网关暂时不可用", Status: hostErr.Status}
	case errors.Is(err, context.DeadlineExceeded):
		return &host.Error{Method: route, Code: codeGatewayUnavailable, Message: "Magpie 网关没有按时响应"}
	}
	return err
}
