package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// The gateway answers a request without a key when it reaches the gateway
// on loopback, carries no forwarding header (X-Forwarded-For, Forwarded,
// X-Real-IP, Cf-Connecting-IP and the like) and does not look like a
// browser (Origin, Sec-Fetch-*, a Mozilla User-Agent): Magpie's
// internal/gateway/lan.go, local and proxied. The requests here send none
// of those and never go through a proxy: a proxy on this computer would
// add X-Forwarded-For and make the request look like one from another
// machine.

// maxGatewayBody bounds a gateway response read whole.
const maxGatewayBody = 16 << 20

// errorBodyLimit bounds the part of an error response kept in host.Error,
// like quota.HTTPError.Body.
const errorBodyLimit = 4096

var gatewayTransport = &http.Transport{
	Proxy:           nil,
	DialContext:     (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:    4,
	IdleConnTimeout: 90 * time.Second,
}

// gatewayClient talks to the gateway directly, never through the
// environment's proxy. It sets no timeout: each request bounds itself
// through its context, and a model stream lives until it is closed.
var gatewayClient = &http.Client{Transport: gatewayTransport}

// Deadlines of the whole Accounts, KeptQuotas, RefreshQuota and Models
// calls: the response headers, the body and the account list a call reads
// beside its own request. The background loop's context ends only when
// the plugin stops, so a gateway that takes a connection and then stops
// answering would hold the loop without them. Tests shorten them.
var (
	accountsTimeout = 20 * time.Second
	keptTimeout     = 20 * time.Second
	listTimeout     = 20 * time.Second
	// refreshLookTimeout bounds what RefreshQuota reads before its
	// refresh: the account list and the kept reading. refreshTimeout
	// bounds the refresh alone and covers the gateway's own 30 seconds
	// for a quota card, which a Claude card spends running Claude Code's
	// /usage. Together they stay under the 50 seconds a tool call through
	// Magpie gets (engine.manualRefreshBudget).
	refreshLookTimeout = 8 * time.Second
	refreshTimeout     = 32 * time.Second
)

// gatewayURL is path on the gateway at h.Addr. An address on every
// interface (":3425", "0.0.0.0:3425") is reached on loopback, as Magpie's
// own plugin host does; an address with a scheme is used as it is.
func (h *Host) gatewayURL(path string) (string, error) {
	addr := strings.TrimSpace(h.Addr)
	if addr == "" {
		return "", errors.New("MAGPIE_ADDR 未设置，找不到 Magpie 网关")
	}
	if strings.Contains(addr, "://") {
		return strings.TrimRight(addr, "/") + path, nil
	}
	if hostPart, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(hostPart); hostPart == "" || ip != nil && ip.IsUnspecified() {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return "http://" + addr + path, nil
}

// newGatewayRequest is a request for path on the gateway, with the headers
// every gateway request carries.
func (h *Host) newGatewayRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	target, err := h.gatewayURL(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		// The error would quote the URL, query included.
		return nil, errors.New("MAGPIE_ADDR 不是可用的网关地址")
	}
	req.Header.Set("User-Agent", "magpie-lamplighter")
	return req, nil
}

// gatewayJSON sends one request without a body to the gateway and decodes
// its JSON answer into out. An unreachable gateway or a status other than
// 2xx is a *host.Error (errors.As); an answer that is not JSON of out's
// shape wraps host.ErrFormat. A request that fails without an answer, or
// whose answer cannot be read, names no URL (failedRequest).
func (h *Host) gatewayJSON(ctx context.Context, method, path string, out any) error {
	route := method + " " + routeOf(path)
	req, err := h.newGatewayRequest(ctx, method, path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", route, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := gatewayClient.Do(req)
	if err != nil {
		failed := failedRequest(ctx, route, err)
		return &unreachable{err: &host.Error{Method: route, Code: "gateway_unreachable", Message: failed.text}, cause: failed.cause}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayBody))
	if err != nil {
		return failedRequest(ctx, route, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gatewayError(route, resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: %w: %v", route, host.ErrFormat, err)
	}
	return nil
}

// requestFailed is a gateway request that got no answer, or whose answer
// could not be read. Its text names the method, the route without its
// query and the kind of failure (failureOf), never the URL: the query of
// a request can name the account, such as the user of a quota refresh,
// and *url.Error quotes the whole URL. A request ended by its context
// wraps context.Canceled or context.DeadlineExceeded, also when the
// context ended with a cause of its own.
type requestFailed struct {
	text  string
	cause error
}

func (f *requestFailed) Error() string { return f.text }
func (f *requestFailed) Unwrap() error { return f.cause }

// failedRequest is requestFailed for err of the request route, such as
// "POST /v1/magpie/quotas/refresh", sent with ctx.
func failedRequest(ctx context.Context, route string, err error) *requestFailed {
	kind, cause := failureOf(ctx, err)
	return &requestFailed{text: route + ": " + kind, cause: cause}
}

// failureOf is the kind of a request sent with ctx that failed with err,
// and the context error that ended it, nil for none. A context that ended
// with a cause (context.WithDeadlineCause, WithCancelCause) has the
// transport return that cause in place of context.DeadlineExceeded or
// context.Canceled, so ctx itself tells how the request ended.
func failureOf(ctx context.Context, err error) (string, error) {
	switch ctx.Err() {
	case context.Canceled:
		return "request canceled", context.Canceled
	case context.DeadlineExceeded:
		return "request timed out", context.DeadlineExceeded
	}
	var netErr net.Error
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "request canceled", context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out", context.DeadlineExceeded
	case errors.As(err, &netErr) && netErr.Timeout():
		return "request timed out", nil
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused", nil
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset", nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed", nil
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "connection failed", nil
	}
	return "request failed", nil
}

// unreachable is the *host.Error of a gateway that gave no answer
// (gateway_unreachable, with the text of failedRequest), and wraps the
// context error that ended the request, if any.
type unreachable struct {
	err   *host.Error
	cause error
}

func (u *unreachable) Error() string { return u.err.Error() }

func (u *unreachable) Unwrap() []error {
	if u.cause == nil {
		return []error{u.err}
	}
	return []error{u.err, u.cause}
}

// gatewayError turns an error response into a host.Error. Message keeps
// the first errorBodyLimit bytes of the body, which is the error object in
// the client's format: {"type":"error","error":{"type","message"}} on
// /v1/messages, {"error":{"message","type","code","param"}} elsewhere
// (Magpie's writeErrorAs). failure.Diagnose and ignite.Classify read it
// from there.
func gatewayError(method string, status int, body []byte) error {
	if len(body) > errorBodyLimit {
		body = body[:errorBodyLimit]
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	return &host.Error{Method: method, Code: "gateway_http_error", Message: message, Status: status}
}

// routeOf is path without its query.
func routeOf(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}
