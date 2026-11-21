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
// uses as window IDs, so its readings would update no window.
func (h *Host) KeptQuotas(ctx context.Context) ([]host.KeptQuota, error) {
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
	// refreshTimeout covers the gateway's own 30 seconds.
	refreshTimeout = 45 * time.Second
)

// ErrRefreshLimited is returned when an account's quota was refreshed too
// recently; see claudeRefreshGap.
var ErrRefreshLimited = errors.New("额度刷新过于频繁")

// RefreshQuota asks Magpie to read the quota of the account pin again. It
// refuses within claudeRefreshGap (Claude) or refreshGap (others) of the
// last attempt for the same account, whatever its result.
func (h *Host) RefreshQuota(ctx context.Context, pin string) error {
	a, err := h.accountByPin(ctx, pin)
	if err != nil {
		return err
	}
	gap := refreshGap
	if a.Provider == "claude" {
		gap = claudeRefreshGap
	}
	st := h.state()
	st.mu.Lock()
	last := st.refreshed[a.Pin]
	if t := now(); !last.IsZero() && t.Sub(last) < gap {
		st.mu.Unlock()
		return fmt.Errorf("%w：%s 在 %s 后才能再次刷新", ErrRefreshLimited, a.Pin, (gap - t.Sub(last)).Round(time.Second))
	}
	st.refreshed[a.Pin] = now()
	st.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	query := url.Values{"provider": {a.Provider}}
	if a.User != "" {
		query.Set("user", a.User)
	}
	var resp struct {
		Data *[]any `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodPost, refreshPath+"?"+query.Encode(), &resp); err != nil {
		return err
	}
	if resp.Data == nil {
		return fmt.Errorf("POST %s: %w: no data list", refreshPath, host.ErrFormat)
	}
	return nil
}
