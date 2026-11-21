package magpie

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// hidden are the parts of a refresh URL of b@example.com that an error
// may not show.
var hidden = []string{"example.com", "b%40", "user=", "provider=", "http://"}

func showsNone(t *testing.T, what string, err error) {
	t.Helper()
	for _, s := range hidden {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("%s: %q shows %q", what, err, s)
		}
	}
}

// A gateway request without an answer names its method and route and why
// it failed, never its URL, whose query can name the account; a request
// ended by its context still tells which way.
func TestGatewayRequestErrorLeavesOutTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client going away once the body is read.
		io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	h := &Host{Addr: strings.TrimPrefix(srv.URL, "http://")}
	path := refreshPath + "?provider=claude&user=b%40example.com"

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := h.gatewayJSON(ctx, http.MethodPost, path, nil)
	var hostErr *host.Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &hostErr) || hostErr.Code != "gateway_unreachable" ||
		!strings.Contains(err.Error(), "POST /v1/magpie/quotas/refresh: request timed out") {
		t.Fatalf("timeout: %v", err)
	}
	showsNone(t, "timeout", err)

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err = h.gatewayJSON(ctx, http.MethodPost, path, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request canceled") {
		t.Fatalf("cancel: %v", err)
	}
	showsNone(t, "cancel", err)

	// Nothing listens on the address.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	closed := &Host{Addr: ln.Addr().String()}
	ln.Close()
	err = closed.gatewayJSON(context.Background(), http.MethodPost, path, nil)
	if !errors.As(err, &hostErr) || hostErr.Code != "gateway_unreachable" || errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "POST /v1/magpie/quotas/refresh: connection") {
		t.Fatalf("closed port: %v", err)
	}
	showsNone(t, "closed port", err)

	// A model stream cancelled before its headers.
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = h.OpenModelStream(ctx, host.ModelRequest{Account: "claude/x", Protocol: "claude", Body: []byte("{}")})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "POST /v1/messages: request canceled") {
		t.Fatalf("model stream: %v", err)
	}
	showsNone(t, "model stream", err)
}

// A refresh whose caller ends while the gateway has not answered returns
// the caller's end as it is, without the account in the URL's query.
func TestRefreshTimeoutLeavesOutTheAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/magpie/accounts":
			w.Write([]byte(claudeOn))
		case "GET /v1/magpie/quotas":
			w.Write([]byte(cardsBefore))
		default:
			io.ReadAll(r.Body)
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	h := &Host{Addr: strings.TrimPrefix(srv.URL, "http://")}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := h.RefreshKept(ctx, "claude/0f0e0d0c0b0a0908")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "POST /v1/magpie/quotas/refresh") {
		t.Fatalf("err = %v", err)
	}
	showsNone(t, "refresh", err)
}

// A context that ends with a cause of its own, as a manual refresh's
// deadline does, has the transport return the cause in place of the
// context's error. The request's error still wraps
// context.DeadlineExceeded or context.Canceled.
func TestGatewayRequestEndedWithACause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	h := &Host{Addr: strings.TrimPrefix(srv.URL, "http://")}
	path := refreshPath + "?provider=claude&user=b%40example.com"
	cause := errors.New("the manual refresh ran out of time")

	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(50*time.Millisecond), cause)
	defer cancel()
	err := h.gatewayJSON(ctx, http.MethodPost, path, nil)
	var hostErr *host.Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &hostErr) || hostErr.Code != "gateway_unreachable" ||
		!strings.Contains(err.Error(), "request timed out") {
		t.Fatalf("deadline: %v", err)
	}
	showsNone(t, "deadline", err)

	ctx, cancelCause := context.WithCancelCause(context.Background())
	time.AfterFunc(50*time.Millisecond, func() { cancelCause(cause) })
	err = h.gatewayJSON(ctx, http.MethodPost, path, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request canceled") {
		t.Fatalf("cancel: %v", err)
	}

	ctx, cancel = context.WithDeadlineCause(context.Background(), time.Now().Add(50*time.Millisecond), cause)
	defer cancel()
	_, err = h.OpenModelStream(ctx, host.ModelRequest{Account: "claude/x", Protocol: "claude", Body: []byte("{}")})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request timed out") {
		t.Fatalf("model stream: %v", err)
	}
}

// A refresh whose caller ends with a cause after the readings before the
// refresh sends none, and its error wraps the cause and
// context.DeadlineExceeded or context.Canceled.
func TestRefreshCallerEndedWithACause(t *testing.T) {
	cause := errors.New("the manual refresh ran out of time")
	for _, deadline := range []bool{true, false} {
		g, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts":        claudeOn,
			"GET /v1/magpie/quotas":          cardsBefore,
			"POST /v1/magpie/quotas/refresh": cardsAfter,
		})
		var ctx context.Context
		var atEOF func()
		end := time.Now().Add(500 * time.Millisecond)
		if deadline {
			c, cancel := context.WithDeadlineCause(context.Background(), end, cause)
			defer cancel()
			ctx, atEOF = c, func() { time.Sleep(time.Until(end) + 20*time.Millisecond) }
		} else {
			c, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ctx, atEOF = c, func() { cancel(cause) }
		}
		old := gatewayClient
		gatewayClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := gatewayTransport.RoundTrip(r)
			if err == nil && r.Method == http.MethodGet && r.URL.Path == "/v1/magpie/quotas" {
				// The kept reading arrives whole; the caller ends right
				// after.
				resp.Body = eofBody{ReadCloser: resp.Body, atEOF: atEOF}
			}
			return resp, err
		})}
		_, _, err := h.RefreshKept(ctx, "claude/0f0e0d0c0b0a0908")
		gatewayClient = old
		want := context.Canceled
		if deadline {
			want = context.DeadlineExceeded
		}
		if !errors.Is(err, want) || !errors.Is(err, cause) || !strings.Contains(err.Error(), cause.Error()) {
			t.Errorf("deadline=%v: err = %v", deadline, err)
		}
		for _, r := range g.requests {
			if r.Method == http.MethodPost {
				t.Errorf("deadline=%v: sent %s %s", deadline, r.Method, r.URL)
			}
		}
	}
}
