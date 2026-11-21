// Package egress sends quota requests through the same network exit that
// Magpie uses for the account.
//
// The host fills Credential.ProxyURL with the account's proxy in Magpie,
// else Magpie's global proxy. An empty proxy uses the environment proxy
// (HTTPS_PROXY and the rest), and "direct" or "none" a direct connection.
// A proxy that cannot be parsed fails the request instead of falling back
// to another exit.
package egress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

const maxResponseBytes = 4 << 20

// Egress implements quota.Doer.
type Egress struct {
	Timeout time.Duration

	mu         sync.Mutex
	transports map[string]*http.Transport
}

// SetTimeout sets the timeout of the requests sent after it returns.
func (e *Egress) SetTimeout(d time.Duration) {
	e.mu.Lock()
	e.Timeout = d
	e.mu.Unlock()
}

// Do sends req through proxyURL; see the package comment.
func (e *Egress) Do(ctx context.Context, proxyURL string, req quota.Request) (quota.Response, error) {
	e.mu.Lock()
	timeout := e.Timeout
	e.mu.Unlock()
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport, err := e.transport(strings.TrimSpace(proxyURL))
	if err != nil {
		return quota.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return quota.Response{}, err
	}
	if req.Header != nil {
		httpReq.Header = req.Header.Clone()
	}
	resp, err := (&http.Client{Transport: transport}).Do(httpReq)
	if err != nil {
		return quota.Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return quota.Response{}, err
	}
	return quota.Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

func (e *Egress) transport(proxyURL string) (*http.Transport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if transport, ok := e.transports[proxyURL]; ok {
		return transport, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	switch strings.ToLower(proxyURL) {
	case "":
		transport.Proxy = http.ProxyFromEnvironment
	case "direct", "none":
		transport.Proxy = nil
	default:
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("代理地址无效，已跳过请求: %s", Redact(proxyURL))
		}
		switch parsed.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("代理地址无效，已跳过请求: %s", Redact(proxyURL))
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	if e.transports == nil {
		e.transports = make(map[string]*http.Transport)
	}
	e.transports[proxyURL] = transport
	return transport, nil
}

// Redact hides the user name and password of a proxy address.
func Redact(proxyURL string) string {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return "[invalid]"
	}
	if parsed.User != nil {
		parsed.User = url.User("***")
	}
	return parsed.String()
}
