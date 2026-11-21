// Package redact removes account emails, credentials and URL parameters
// from texts that leave the process or are kept on disk: log lines, the
// errors of account state, events and history, API and MCP answers and
// notifications. The callers redact at those exits, on the text alone, so
// that the errors themselves stay whole for errors.Is and errors.As.
package redact

import (
	"io"
	"regexp"
	"strings"
)

// Omitted stands for a value left out.
const Omitted = "…"

var (
	// urlPattern is an absolute URL: a scheme, "://" and everything up to
	// a space, a quote or an angle bracket.
	urlPattern = regexp.MustCompile("(?i)\\b[a-z][a-z0-9+.\\-]*://[^\\s\"'<>`]+")
	// emailPattern is an email: a local part, "@" and a domain with a
	// dot. A model id such as "claude-sonnet-4@20250514" has no dot after
	// "@" and is left as it is.
	emailPattern = regexp.MustCompile(`[^\s@"'<>()\[\]{},;:（）：，；“”。、]+@[^\s@"'<>()\[\]{},;:（）：，；“”。、.]+(?:\.[^\s@"'<>()\[\]{},;:（）：，；“”。、.]+)+`)
	// schemePattern is an HTTP authorization scheme and its credential.
	schemePattern = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9\-._~+/]+=*`)
	// jwtPattern is a JSON Web Token: its header starts with eyJ, the
	// base64url of `{"`.
	jwtPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]*(?:\.[A-Za-z0-9_\-]*){0,2}`)
	// fieldPattern is a field that holds a credential, with its value:
	// access_token=…, "refresh_token": "…", api_key: ….
	fieldPattern = regexp.MustCompile(`(?i)\b(access_token|refresh_token|id_token|client_secret|api[_-]?key|private_key|password|secret|token)(["']?\s*[:=]\s*["']?)([^\s"'&,;}\]]+)`)
	// longPattern is a run of 32 or more base64url or hex characters, as
	// tokens, keys and their hashes are written.
	longPattern = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`)
)

// Text is s without what may identify an account or let someone act as
// it:
//
//   - a URL keeps its scheme, host and path, without user info, query or
//     fragment;
//   - an email keeps only the last two characters of its local part,
//     such as "***rk" (Email);
//   - the credential of "Bearer" or "Basic", a JWT, the value of a field
//     named like access_token, refresh_token, api_key or password, and any
//     run of 32 or more base64url or hex characters become "…".
//
// Everything else, line breaks and JSON quoting included, stays as it is,
// so that a redacted JSON error body still parses. Text of a redacted text
// is that text.
func Text(s string) string {
	if s == "" {
		return s
	}
	s = urlPattern.ReplaceAllStringFunc(s, URL)
	s = emailPattern.ReplaceAllStringFunc(s, Email)
	s = schemePattern.ReplaceAllString(s, "${1}${2}"+Omitted)
	s = jwtPattern.ReplaceAllString(s, Omitted)
	s = fieldPattern.ReplaceAllString(s, "${1}${2}"+Omitted)
	s = longPattern.ReplaceAllString(s, Omitted)
	return s
}

// URL is the absolute URL u without user info, query and fragment. The
// scheme, host and path are kept as written.
func URL(u string) string {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return u
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	if cut := strings.IndexAny(tail, "?#"); cut >= 0 {
		tail = tail[:cut]
	}
	return scheme + "://" + authority + tail
}

// Email keeps the last two characters of the local part of email, quotes
// left out, such as "***rk": the domain and the length of the local part
// are not shown.
func Email(email string) string {
	local := email
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		local = email[:at]
	}
	runes := []rune(strings.ReplaceAll(local, "\"", ""))
	if len(runes) > 2 {
		runes = runes[len(runes)-2:]
	}
	return "***" + string(runes)
}

// Params is a copy of params with each value redacted (Text), nil for
// nil.
func Params(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = Text(v)
	}
	return out
}

// Writer is w with each write redacted (Text). The log package writes
// each entry with one call, so every log line is redacted whole.
func Writer(w io.Writer) io.Writer { return writer{w} }

type writer struct{ w io.Writer }

func (r writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(r.w, Text(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
