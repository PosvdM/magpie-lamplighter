package providers

import (
	"regexp"
	"strings"

	"github.com/PosvdM/magpie-lamplighter/internal/models"
)

// Antigravity's quota is read only from Magpie, which has the agy-cli plugin
// run the official agy CLI's /usage; Lamplighter sends no request to Google.
// The provider therefore has no Fetch, and package magpie converts the
// plugin's windows with AntigravityPool.
var antigravity = &Provider{
	ID: "antigravity",
	// Magpie serves Antigravity accounts through the agy-cli plugin
	// (@magpie-community/opencode-agy-cli-auth), whose provider id they
	// carry.
	Aliases: []string{"agy-cli"},
	Title:   "Antigravity",
	Monitor: true,
	Plan:    antigravityPlan,
	Ignition: &Ignition{
		Protocol: "openai",
		Body: func(model string) map[string]any {
			return chatBody(model, map[string]any{"temperature": 0, "reasoning_effort": "none"})
		},
		Candidates: antigravityCandidates,
		Hint:       Text{Zh: "最新的 Flash", En: "latest Flash"},
		// The settings show the Gemini model, which matches "latest Flash".
		HintGroup: "Gemini",
		Groups:    []string{"Gemini", "Claude / GPT"},
		GroupsHint: Text{
			Zh: "Antigravity 按模型分成多个独立的 5 小时额度组。只有勾选的额度组会自动点火；Claude / GPT 额度组较小，默认不点。",
			En: "Antigravity splits quota into separate 5-hour groups by model. Only checked groups are ignited; the Claude / GPT group is small and off by default.",
		},
		// The Claude / GPT group is much smaller than the Gemini one, so only
		// Gemini is ignited unless groups says otherwise.
		DefaultGroups: []string{"Gemini"},
	},
}

// antigravityCandidates orders the models of one quota group: Flash first
// for Gemini; Haiku, Sonnet, Opus and GPT-OSS for Claude / GPT.
func antigravityCandidates(groupLabel string, available []string) []string {
	noThinking := func(m string) bool { return !strings.Contains(m, "thinking") }
	group := strings.ToLower(groupLabel)
	var candidates []string
	for _, model := range available {
		if !strings.Contains(strings.ToLower(model), "image") {
			candidates = append(candidates, model)
		}
	}
	var out []string
	if strings.Contains(group, "gemini") {
		gemini := models.Newest(candidates, func(m string) bool { return strings.Contains(m, "gemini") })
		out = append(out, models.Newest(gemini, func(m string) bool { return strings.Contains(m, "flash") })...)
		out = append(out, models.Newest(gemini, func(m string) bool { return !strings.Contains(m, "pro") })...)
		out = append(out, models.Newest(gemini, func(m string) bool { return strings.Contains(m, "pro") })...)
	}
	if strings.Contains(group, "claude") || strings.Contains(group, "gpt") {
		scoped := models.Newest(candidates, func(m string) bool {
			return strings.Contains(m, "claude") || strings.Contains(m, "gpt")
		})
		out = append(out, models.InOrder(scoped,
			func(m string) bool { return strings.Contains(m, "haiku") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "sonnet") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "opus") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "gpt-oss") },
			noThinking,
		)...)
	}
	return out
}

// AntigravityPoolGroup is the quota group and window IDs of one Antigravity
// quota pool.
type AntigravityPoolGroup struct {
	// Key is the group key without the provider prefix, such as
	// "antigravity:gemini-models".
	Key string
	// SourceLabel names the group, such as "Gemini" or "Claude / GPT".
	SourceLabel string
	// FiveHour and Weekly are the IDs of the pool's 5-hour and 7-day
	// windows.
	FiveHour, Weekly string
}

var poolSlug = regexp.MustCompile(`[^a-z0-9]+`)

// AntigravityPool returns the quota group of the pool that the agy-cli
// plugin names pool: Google's group name without " models", with " and "
// written " & ", such as "Gemini" or "Claude & GPT". The keys and window
// IDs of Gemini and Claude & GPT are those of the history and the settings
// (hidden_groups, ignition groups); another pool gets a key and IDs made
// from its name. ok is false for an empty pool name.
func AntigravityPool(pool string) (g AntigravityPoolGroup, ok bool) {
	name := strings.Join(strings.Fields(pool), " ")
	slug := func(s string) string {
		return strings.Trim(poolSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	}
	// The key keeps Google's group name: "Claude and GPT models" gives
	// "claude-and-gpt-models".
	base := slug(strings.ReplaceAll(name, "&", " and "))
	if slug(name) == "" {
		return AntigravityPoolGroup{}, false
	}
	g = AntigravityPoolGroup{Key: "antigravity:" + base + "-models", SourceLabel: name}
	ids := base
	switch strings.ToLower(name) {
	case "gemini":
		g.SourceLabel, ids = "Gemini", "gemini"
	case "claude & gpt":
		// Google's buckets of this pool are "3p-5h" and "3p-weekly".
		g.SourceLabel, ids = "Claude / GPT", "3p"
	}
	g.FiveHour, g.Weekly = ids+"-5h", ids+"-weekly"
	return g, true
}
