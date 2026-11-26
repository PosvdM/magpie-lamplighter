package providers

import (
	"testing"
	"time"
)

func TestCodexPlansAreWeightedByPrice(t *testing.T) {
	for raw, want := range map[string]Plan{
		"plus":                        {"Plus", 1},
		"team":                        {"Business", 1},
		"self_serve_business_prolite": {"Business Premium", 5},
		"prolite":                     {"Pro $100", 5},
		"pro":                         {"Pro $200", 10},
		"promax":                      {"Pro $500", 25},
		"enterprise":                  {"Enterprise", 1},
		"free":                        {"Free", 0.1},
		"go":                          {"Go", 0.1},
		"something_new":               {"something_new", 1},
	} {
		if got := PlanOf("codex", raw); got != want {
			t.Errorf("%s: got %+v, want %+v", raw, got, want)
		}
	}
	if got := PlanOf("codex", ""); got != (Plan{Weight: 1}) {
		t.Errorf("unknown plan: %+v", got)
	}
	if got := PlanOf("unknown", "pro"); got != (Plan{Weight: 1}) {
		t.Errorf("provider without plans: %+v", got)
	}
}

func TestParseCodexUsageReadsThePlan(t *testing.T) {
	payload := decode(t, `{"plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":60}}}`)
	groups, err := ParseCodexUsage(payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if groups[0].Plan != "prolite" {
		t.Fatalf("plan %q", groups[0].Plan)
	}
}

func TestClaudePlansFromTheProfile(t *testing.T) {
	for body, want := range map[string]Plan{
		// Measured on a Pro account on 2026-10-20.
		`{"account":{"has_claude_pro":true,"has_claude_max":false},"organization":{"organization_type":"claude_pro","rate_limit_tier":"default_claude_ai","subscription_status":"active"}}`: {"Pro", 1},
		`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x"}}`:                                                                                     {"Max 5x", 5},
		`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`:                                                                                    {"Max 20x", 20},
		`{"organization":{"organization_type":"claude_team","rate_limit_tier":"default_claude_ai"}}`:                                                                                        {"Team", 1},
		`{"account":{"has_claude_max":true},"organization":{}}`:                                                                                                                             {"Max", 5},
	} {
		raw, err := ParseClaudeProfile(decode(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if got := PlanOf("claude", raw); got != want {
			t.Errorf("%s: got %+v, want %+v", body, got, want)
		}
	}
	if _, err := ParseClaudeProfile(decode(t, `{"account":{}}`)); err == nil {
		t.Error("a profile without a plan is an error")
	}
}

func TestAntigravityPlansFromMagpie(t *testing.T) {
	for raw, want := range map[string]Plan{
		"Google AI Pro":              {"Pro", 1},
		"Google AI Ultra":            {"Ultra", 1},
		"google ai  Plus":            {"Plus", 1},
		"Antigravity Starter Quota":  {"Free", 1},
		"g1-pro-tier":                {"Pro", 1},
		"free-tier":                  {"Free", 1},
		"standard-tier":              {"standard-tier", 1},
		"Gemini Code Assist Premium": {"Gemini Code Assist Premium", 1},
	} {
		if got := PlanOf("antigravity", raw); got != want {
			t.Errorf("%q: got %+v, want %+v", raw, got, want)
		}
	}
	// The tier ids a 0.1.7 state holds name the same plans, so reading the
	// plan from Magpie announces no change.
	if PlanOf("antigravity", "g1-pro-tier").Name != PlanOf("antigravity", "Google AI Pro").Name ||
		PlanOf("antigravity", "free-tier").Name != PlanOf("antigravity", "Antigravity Starter Quota").Name {
		t.Error("the tier id and the tier name of one plan differ")
	}
}
