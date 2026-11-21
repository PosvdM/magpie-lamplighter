package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

const replyLimit = 4096
const streamFrameLimit = 1 << 20

// Only protocol identifiers in this allowlist may reach persisted errors.
// Never classify or retain free-form provider messages: they can contain secrets.
//
// "rate_limit" is Claude Code's name of a rate-limited turn, which Magpie's
// Claude Code bridge writes as the stream error's code (internal/gateway
// claude_subscription.go, the error kind of the failed turn).
var streamErrorCodes = map[string]int{
	"rate_limit_error": 429, "rate_limit_exceeded": 429, "insufficient_quota": 429, "quota_exceeded": 429,
	"usage_limit_reached": 429, "rate_limit": 429,
	"authentication_error": 401, "invalid_api_key": 401, "unauthorized": 401,
	"permission_error": 403, "permission_denied": 403, "forbidden": 403,
	"not_found_error": 404, "not_found": 404, "model_not_found": 404,
	"invalid_request_error": 400, "invalid_request": 400, "invalid_model": 400,
	"unsupported_model": 400, "model_not_supported": 400,
	"overloaded_error": 529, "server_is_overloaded": 529,
	"server_error": 500, "api_error": 500, "internal_server_error": 500,
}

// magpieRetryStatus matches the fixed suffix Magpie's gateway appends to the
// message of a stream error whose status it can no longer send, when the
// status is 408, 429 or 5xx (internal/gateway saysPassing): "(429: you can
// retry your request)". Only this anchored marker is read from a message.
var magpieRetryStatus = regexp.MustCompile(`\(([0-9]{3}): you can retry your request\)\s*$`)

// genericStreamCodes are the types and codes Magpie's encoders write when
// the upstream error named none (internal/gateway: the Claude and Chat
// encoders' api_error, the Responses encoder's server_error). They tell
// nothing about the status, but mark an error Magpie wrote; an error
// without either is not read for words.
var genericStreamCodes = map[string]bool{"": true, "api_error": true, "server_error": true}

// magpiePassingWords are the words for which Magpie's saysPassing leaves out
// the status suffix, because the message already says the error passes
// (passingSaid). Only these decide a status, and only for an error with a
// generic code; the message itself is dropped. Other passing words, such
// as "timed out" or "server error", keep the generic code's 500.
var magpiePassingWords = []struct {
	pattern *regexp.Regexp
	status  int
}{
	{regexp.MustCompile(`(?i)\b429\b|rate.?limit|too many requests`), 429},
	{regexp.MustCompile(`(?i)overloaded`), 529},
}

// claudeUsageLimit matches Claude Code's fixed wording of a used-up
// subscription, "Claude AI usage limit reached|<unix reset>", which
// Magpie's Claude Code bridge passes on as the message of a stream error
// without a status when the turn fails after the stream started. Magpie
// reads the same marker for the reset (internal/gateway routing.go,
// resetsWords). Only the marker is read; the message is dropped.
var claudeUsageLimit = regexp.MustCompile(`(?i)\busage limit reached\|[0-9]{10}\b`)

// magpieHeldOpening matches the opening of Magpie's cappedError: every
// account that could take the request is held at a cap set in Magpie, or
// set not to spend beyond its allowance. It is Magpie's own answer, no
// rate limit of the vendor; a vendor's "usage limit reached|<unix>" does
// not match.
var magpieHeldOpening = regexp.MustCompile(`^(?:X-Magpie-Account: )?(usage cap reached|spend cap reached|usage limit reached): every (?:account|key) `)

// streamAppealURL allowlists the appeal forms of suspended accounts kept from
// a stream error, such as Google's https://forms.gle/... link.
var streamAppealURL = regexp.MustCompile(`^https://(?:forms\.gle/[A-Za-z0-9]{1,64}|support\.google\.com/[A-Za-z0-9/_-]{1,128})$`)

var streamErrorStatusPrefix = regexp.MustCompile(`(?i)^(?:http(?: status)?|status(?: code)?)[: ]+([0-9]{3})\b`)

func allowedStreamStatus(v any) int {
	var status int
	switch value := v.(type) {
	case float64:
		if value != float64(int(value)) {
			return 0
		}
		status = int(value)
	case string:
		status, _ = strconv.Atoi(value)
	}
	switch status {
	case 400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504, 529:
		return status
	}
	return 0
}

// streamErrorText handles the host bridge's string-only error channel. JSON
// envelopes and anchored HTTP/status prefixes supply metadata; all other text
// is discarded, including messages that merely mention a status or model.
func streamErrorText(raw string) error {
	if len(raw) > streamFrameLimit {
		return streamError(nil)
	}
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return streamError(v)
	}
	if match := streamErrorStatusPrefix.FindStringSubmatch(strings.TrimSpace(raw)); match != nil {
		// "HTTP 403: {...}" also carries the error object.
		rest := strings.TrimSpace(raw)[len(match[0]):]
		if start := strings.IndexByte(rest, '{'); start >= 0 && json.Unmarshal([]byte(rest[start:]), &v) == nil && v != nil {
			v["status_code"] = match[1]
			return streamError(v)
		}
		return streamError(map[string]any{"status": match[1]})
	}
	return streamError(nil)
}

func streamError(v map[string]any) error {
	detail := object(v["error"])
	response := object(v["response"])
	if detail == nil {
		detail = object(response["error"])
	}
	if detail == nil {
		detail = v
	}
	status := 0
	for _, envelope := range []map[string]any{v, response, detail} {
		for _, key := range []string{"status_code", "status", "http_status", "code"} {
			if status == 0 {
				status = allowedStreamStatus(envelope[key])
			}
		}
	}
	rawMessage := str(detail["message"])
	if status == 0 {
		if match := magpieRetryStatus.FindStringSubmatch(rawMessage); match != nil {
			status = allowedStreamStatus(match[1])
		}
	}
	// Magpie's own hold is read first: its "usage limit reached: every
	// account" is no limit of the vendor.
	held := magpieHeldOpening.FindStringSubmatch(rawMessage)
	if status == 0 && held == nil && claudeUsageLimit.MatchString(rawMessage) {
		status = 429
	}
	explicit := status
	if held != nil {
		// Worded like the pinned refusal before the headers, so that
		// ignite.PinHeld and failure.Diagnose treat both alike.
		if status == 0 {
			status = 429
		}
		return &host.Error{Method: "model stream", Code: "magpie_account_held", Status: status,
			Message: "ignition provider stream error: X-Magpie-Account: " + held[1] + ": every account is held by Magpie"}
	}
	// A status read from Magpie's passing words; see magpiePassingWords.
	worded := 0
	code, typ := str(detail["code"]), str(detail["type"])
	if status == 0 && genericStreamCodes[code] && genericStreamCodes[typ] && code+typ != "" {
		for _, words := range magpiePassingWords {
			if words.pattern.MatchString(rawMessage) {
				status, worded = words.status, words.status
				break
			}
		}
	}
	for _, key := range []string{"code", "type"} {
		if status == 0 {
			status = streamErrorCodes[str(detail[key])]
		}
	}
	// A disabled account keeps its allowlisted code, the matching constant
	// of failure.SuspendedPhrase and an allowlisted appeal form, so that
	// failure.Diagnose sorts it like the same HTTP error. The provider's
	// message itself is still dropped.
	var identifiers []string
	addSuspended := func(value string) {
		if failure.SuspendedCode(value) && !slices.Contains(identifiers, value) {
			identifiers = append(identifiers, value)
		}
	}
	appeal := ""
	for _, key := range []string{"code", "type", "status", "reason"} {
		addSuspended(str(detail[key]))
	}
	details, _ := detail["details"].([]any)
	for _, item := range details {
		addSuspended(str(object(item)["reason"]))
		if url := str(object(object(item)["metadata"])["appeal_url"]); appeal == "" && streamAppealURL.MatchString(url) {
			appeal = url
		}
	}
	message := "ignition provider stream error"
	// A phrase alone cannot tell a suspension from a rate limit or a
	// passing error that mentions the account: those stay ordinary
	// failures.
	phrase := failure.SuspendedPhrase(rawMessage)
	if phrase != "" && worded == 0 && (explicit == 0 || explicit == 400 || explicit == 401 || explicit == 403) {
		message += ": " + phrase
	} else {
		phrase = ""
	}
	switch {
	case len(identifiers) == 0 && phrase == "":
		appeal = ""
	case explicit == 0 && worded == 0:
		// Without an explicit status, a suspension is sorted like
		// Google's 403 rather than by its type: OpenAI sends
		// account_deactivated as invalid_request_error, which would read
		// as a rejected model.
		status = 403
	}
	for _, key := range []string{"code", "type"} {
		value := str(detail[key])
		if _, ok := streamErrorCodes[value]; ok {
			identifiers = append(identifiers, value)
		}
	}
	errCode := "provider_stream_error"
	if len(identifiers) > 0 {
		errCode = strings.Join(identifiers, "/")
	}
	return &host.Error{Method: "model stream", Code: errCode, Status: status, Message: message, AppealURL: appeal}
}

// streamCapture parses the gateway's SSE body without retaining raw responses or
// opaque reasoning. Only visible text is included in the bounded log reply.
type streamCapture struct {
	pending      []byte
	eventData    []byte
	reply        strings.Builder
	replyChars   int
	output       bool
	onOutput     func()
	terminal     bool
	chatFinished bool
	usage        map[string]any
	usageFinal   bool
}

func (s *streamCapture) text(v string) {
	s.markOutput(v)
	n := utf8.RuneCountInString(v)
	remaining := replyLimit - s.replyChars
	if remaining > 0 {
		r := []rune(v)
		if len(r) > remaining {
			r = r[:remaining]
		}
		s.reply.WriteString(string(r))
	}
	s.replyChars += n
}

func (s *streamCapture) markOutput(v string) {
	if v == "" || s.output {
		return
	}
	s.output = true
	if s.onOutput != nil {
		s.onOutput()
	}
}

// feedChunk takes the next bytes of the body. The gateway answers all three
// protocols with standard SSE, split at any byte: Chat chunks as data lines
// ending in data: [DONE], Claude and Responses as their events.
func (s *streamCapture) feedChunk(_ string, payload []byte) error {
	return s.feed(payload)
}

func (s *streamCapture) feed(payload []byte) error {
	// Standard SSE may split JSON, UTF-8, CRLF, or metadata at any byte.
	// Preserve the current line until its delimiter arrives.
	for len(payload) > 0 {
		i := bytes.IndexByte(payload, '\n')
		if i < 0 {
			if len(s.pending)+len(payload) > streamFrameLimit {
				return errors.New("ignition stream frame too large")
			}
			s.pending = append(s.pending, payload...)
			return nil
		}
		if len(s.pending)+i > streamFrameLimit {
			return errors.New("ignition stream frame too large")
		}
		s.pending = append(s.pending, payload[:i]...)
		line := bytes.TrimSuffix(s.pending, []byte{'\r'})
		if err := s.line(line); err != nil {
			return err
		}
		s.pending = s.pending[:0]
		payload = payload[i+1:]
	}
	return nil
}

func (s *streamCapture) line(line []byte) error {
	if len(line) == 0 {
		data := s.eventData
		s.eventData = nil
		return s.event(data)
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(s.eventData)+len(data)+1 > streamFrameLimit {
			return errors.New("ignition stream frame too large")
		}
		if len(s.eventData) > 0 {
			s.eventData = append(s.eventData, '\n')
		}
		s.eventData = append(s.eventData, data...)
	}
	return nil
}

func (s *streamCapture) finish() error {
	if len(s.pending) > 0 {
		line := bytes.TrimSuffix(s.pending, []byte{'\r'})
		s.pending = nil
		if err := s.line(line); err != nil {
			return err
		}
	}
	if err := s.line(nil); err != nil {
		return err
	}
	// A Chat stream that ends without [DONE] still ends at EOF after its
	// finish_reason. Wait for EOF so a separate final usage chunk is not
	// discarded.
	if s.chatFinished {
		s.terminal = true
	}
	return nil
}

func (s *streamCapture) event(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		s.terminal = true
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return errors.New("invalid ignition stream JSON")
	}
	if v["error"] != nil {
		return streamError(v)
	}
	typ, _ := v["type"].(string)
	switch typ {
	case "error", "response.failed":
		return streamError(v)
	case "response.completed":
		s.terminal = true
		s.recordUsage(object(v["response"])["usage"], true)
	case "response.incomplete":
		s.recordUsage(object(v["response"])["usage"], false)
		return errors.New("ignition provider stream incomplete")
	case "response.output_text.delta":
		s.text(str(v["delta"]))
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
		s.text(str(v["delta"]))
	case "content_block_delta":
		d := object(v["delta"])
		switch str(d["type"]) {
		case "text_delta":
			s.text(str(d["text"]))
		case "thinking_delta":
			s.text(str(d["thinking"]))
		case "input_json_delta":
			s.text(str(d["partial_json"]))
		}
	case "message_start":
		s.recordUsage(object(v["message"])["usage"], false)
	case "message_delta":
		s.recordUsage(v["usage"], true)
	case "message_stop":
		s.terminal = true
	}
	// OpenAI chat completion chunks (also used by Antigravity).
	if choices, ok := v["choices"].([]any); ok {
		for _, c := range choices {
			if str(object(c)["finish_reason"]) != "" {
				s.chatFinished = true
			}
			d := object(object(c)["delta"])
			s.text(str(d["content"]))
			for _, key := range []string{"reasoning", "reasoning_content"} {
				s.text(str(d[key]))
			}
			calls, _ := d["tool_calls"].([]any)
			for _, call := range calls {
				s.text(str(object(object(call)["function"])["arguments"]))
			}
			s.text(str(object(d["function_call"])["arguments"]))
		}
		// The gateway can synthesize missing Chat counters as zero or translate
		// token categories. Field presence cannot establish upstream completeness.
		s.recordUsage(v["usage"], false)
	}
	return nil
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string            { s, _ := v.(string); return s }

// completeUsage describes final host-protocol counters for native ignition
// routes, not independently verified upstream accounting. Translated routes
// remain partial even when their host payload contains both counters.
func (s *streamCapture) completeUsage(provider, protocol string) bool {
	native := (provider == "claude" && protocol == "claude") ||
		(provider == "codex" && protocol == "openai-response")
	_, input := s.usage["input_tokens"]
	_, output := s.usage["output_tokens"]
	return native && s.usageFinal && input && output
}

// recordUsage keeps allowlisted counters supplied by the gateway, which may
// include translated or synthesized values. The plugin does not infer totals
// or turn absent values into zero.
func (s *streamCapture) recordUsage(v any, final bool) {
	m := object(v)
	observed := false
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if n, ok := m[key].(float64); ok && n >= 0 {
			if s.usage == nil {
				s.usage = map[string]any{}
			}
			s.usage[key] = n
			observed = true
		}
	}
	for _, key := range []string{"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"} {
		details := map[string]any{}
		for _, field := range []string{"cached_tokens", "reasoning_tokens", "audio_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens"} {
			if n, ok := object(m[key])[field].(float64); ok && n >= 0 {
				details[field] = n
			}
		}
		if len(details) > 0 {
			if s.usage == nil {
				s.usage = map[string]any{}
			}
			s.usage[key] = details
			observed = true
		}
	}
	if observed && final {
		s.usageFinal = true
	}
}
