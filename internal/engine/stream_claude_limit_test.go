package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
)

// claudeBridgeErrorEvent is the error event Magpie's Anthropic encoder
// writes when a Claude Code turn fails after the stream started
// (internal/gateway claude_subscription.go: the result line with is_error
// becomes a KError with Claude Code's error kind as Code and its
// api_error_status, 0 when Claude Code got none; anthropic.go at KError:
// type api_error, the message, and code when the kind is set). Without a
// status saysPassing adds no suffix.
func claudeBridgeErrorEvent(message, code string) string {
	quoted := strings.ReplaceAll(message, `"`, `\"`)
	codeField := ""
	if code != "" {
		codeField = `,"code":"` + code + `"`
	}
	return "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"" + quoted + "\"" + codeField + "}}\n\n"
}

// A rate limit of the Claude Code bridge without a status or suffix is a
// rate limit: Claude Code's error kind "rate_limit" as the code, its
// "usage limit reached|<unix>" as the message, or "Claude AI usage limit
// reached" alone as the message of an api_error. Magpie's own hold stays a
// hold even with the code.
func TestClaudeBridgeRateLimitInStream(t *testing.T) {
	for _, tc := range []struct {
		name, message, code string
		status              int
		kind                ignite.Kind
		reason              string
		held                bool
	}{
		{"error kind rate_limit", "API Error: SECRET Request rejected", "rate_limit", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"usage limit without suffix", "Claude AI usage limit reached|1790000000", "", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"usage limit with kind", "Claude AI usage limit reached|1790000000", "rate_limit", 429, ignite.Hard, failure.ReasonRateLimited, false},
		// Claude Code's wording without the reset, the whole message of an
		// api_error without a status or code.
		{"usage limit without reset", "Claude AI usage limit reached", "", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"usage limit without reset, full stop", "Claude AI usage limit reached.", "", 429, ignite.Hard, failure.ReasonRateLimited, false},
		// Only the whole message counts.
		{"usage limit within other words", "SECRET Claude AI usage limit reached for SECRET", "", 500, ignite.Transient, "", false},
		{"other kind", "API Error: SECRET broke", "server_error", 500, ignite.Transient, "", false},
		{"no kind", "SECRET something broke", "", 500, ignite.Transient, "", false},
		// A pipe and a time anywhere else is no marker.
		{"no reset after the pipe", "SECRET usage limit reached|soon", "", 500, ignite.Transient, "", false},
		// Magpie holding every account (cappedError), also with Claude
		// Code's kind as the code.
		{"Magpie hold with rate_limit", `usage limit reached: every account that serves "claude/x" is held by magpie — SECRET. magpie uses it again once that window renews`, "rate_limit", 429, ignite.Transient, "", true},
		{"Magpie hold beside the marker", `usage limit reached: every account that serves "claude/x" is held by magpie — SECRET usage limit reached|1790000000`, "", 429, ignite.Transient, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s streamCapture
			err := s.feed([]byte(claudeBridgeErrorEvent(tc.message, tc.code)))
			var typed *host.Error
			if !errors.As(err, &typed) {
				t.Fatalf("err=%v", err)
			}
			d := failure.Diagnose(err)
			if typed.Status != tc.status || ignite.Classify(err) != tc.kind || d.Reason != tc.reason || ignite.PinHeld(err) != tc.held || ignite.ModelRejected(err) {
				t.Fatalf("err=%v status=%d kind=%v diagnosis=%+v held=%v", err, typed.Status, ignite.Classify(err), d, ignite.PinHeld(err))
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(d.Summary, "SECRET") || strings.Contains(err.Error(), "1790000000") || strings.Contains(err.Error(), "claude/x") {
				t.Fatalf("the message reached the error: %v / %q", err, d.Summary)
			}
		})
	}
}
