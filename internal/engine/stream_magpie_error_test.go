package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
)

// Error events in the shapes of Magpie's encoders (internal/gateway
// streamError: anthropic.go, chat.go and responses.go at KError). The
// encoders write api_error or server_error unless the upstream error named
// a type, and saysPassing adds "(<status>: you can retry your request)"
// only when the message has no passing words of its own.
func magpieErrorEvents(message string) map[string]string {
	quoted := strings.ReplaceAll(message, `"`, `\"`)
	return map[string]string{
		"claude": "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"" + quoted + "\"}}\n\n",
		"chat":   "data: {\"error\":{\"message\":\"" + quoted + "\",\"type\":\"api_error\"}}\n\n",
		"responses": "event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"failed\",\"model\":\"codex/gpt-6-luna\"," +
			"\"error\":{\"code\":\"server_error\",\"message\":\"" + quoted + "\"}}}\n\n",
	}
}

func TestStreamErrorsInMagpieShapes(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		status        int
		kind          ignite.Kind
		reason        string
		held          bool
	}{
		// saysPassing leaves out the suffix: the words say it passes.
		{"rate limit words", "SECRET Rate limit reached for requests", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"too many requests", "SECRET too many requests, slow down", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"status in the words", "SECRET upstream said 429", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"overloaded", "SECRET Overloaded", 529, ignite.Transient, "", false},
		{"other passing words", "SECRET request timed out", 500, ignite.Transient, "", false},
		// The suffix when the words say nothing.
		{"vendor usage limit with suffix", "Claude AI usage limit reached|1790000000 SECRET (429: you can retry your request)", 429, ignite.Hard, failure.ReasonRateLimited, false},
		{"unavailable with suffix", "SECRET no first token in 30s (503: you can retry your request)", 503, ignite.Transient, "", false},
		{"no passing words", "SECRET something broke", 500, ignite.Transient, "", false},
		// Magpie holding the account (cappedError), with and without the
		// suffix: no rate limit of the vendor.
		{"Magpie usage limit", `usage limit reached: every account that serves "claude/x" is held by magpie — SECRET rests (rate limit). magpie uses it again once that window renews`, 429, ignite.Transient, "", true},
		{"Magpie usage cap", `usage cap reached: every account that serves "claude/x" is held at the usage cap set on it in magpie — SECRET (429: you can retry your request)`, 429, ignite.Transient, "", true},
		{"Magpie spend cap", `spend cap reached: every key that serves "x" has spent the spend cap set on it in magpie — SECRET (429: you can retry your request)`, 429, ignite.Transient, "", true},
		// A suspension phrase beside rate limit words cannot be told
		// apart: it stays an ordinary failure, no suspension.
		{"phrase and rate limit", "SECRET Your account has been suspended: rate limit", 429, ignite.Hard, failure.ReasonRateLimited, false},
	} {
		for channel, event := range magpieErrorEvents(tc.message) {
			t.Run(tc.name+"/"+channel, func(t *testing.T) {
				var s streamCapture
				err := s.feed([]byte(event))
				var typed *host.Error
				if !errors.As(err, &typed) {
					t.Fatalf("err=%v", err)
				}
				d := failure.Diagnose(err)
				if typed.Status != tc.status || ignite.Classify(err) != tc.kind || d.Reason != tc.reason || ignite.PinHeld(err) != tc.held || ignite.ModelRejected(err) {
					t.Fatalf("err=%v status=%d kind=%v diagnosis=%+v held=%v", err, typed.Status, ignite.Classify(err), d, ignite.PinHeld(err))
				}
				if strings.Contains(err.Error(), "SECRET") || strings.Contains(d.Summary, "SECRET") || strings.Contains(err.Error(), "claude/x") {
					t.Fatalf("the message reached the error: %v / %q", err, d.Summary)
				}
			})
		}
	}
}

// An upstream type that Magpie passes on decides over the words.
func TestStreamErrorTypeBeatsWords(t *testing.T) {
	var s streamCapture
	err := s.feed([]byte("data: {\"error\":{\"message\":\"SECRET rate limit\",\"type\":\"invalid_request_error\",\"code\":\"model_not_found\"}}\n\n"))
	if host.StatusOf(err) != 404 || !ignite.ModelRejected(err) {
		t.Fatalf("err=%v", err)
	}
}
