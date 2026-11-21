package magpie

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

func testHost(t *testing.T, handler http.HandlerFunc) *Host {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Host{Addr: strings.TrimPrefix(server.URL, "http://")}
}

// modelsSample is the shape of Magpie's GET /v1/models (gateway.go
// listModels, modelObject), trimmed to the fields Lamplighter reads.
const modelsSample = `{"object":"list","has_more":false,"data":[
{"id":"codex/gpt-5.5","object":"model","owned_by":"codex","reasoning":true},
{"id":"claude/claude-haiku-4-5","object":"model","owned_by":"claude"},
{"id":"gemini-3-flash","object":"model","owned_by":"antigravity"},
{"id":"group/fast","object":"model","owned_by":"magpie"},
{"id":"codex/gpt-5.5","object":"model","owned_by":"codex"}]}`

func TestModels(t *testing.T) {
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "" {
			t.Errorf("request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		io.WriteString(w, modelsSample)
	})
	got, err := h.Models(context.Background())
	want := []string{"codex/gpt-5.5", "claude/claude-haiku-4-5", "antigravity/gemini-3-flash", "group/fast"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestModelsFormatAndStatus(t *testing.T) {
	for _, body := range []string{`{"object":"list"}`, `[]`, `{"data":[{"object":"model"}]}`, `not json`} {
		h := testHost(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
		if _, err := h.Models(context.Background()); !errors.Is(err, host.ErrFormat) {
			t.Errorf("%s: %v", body, err)
		}
	}
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "magpie isn't shared on the local network\n")
	})
	_, err := h.Models(context.Background())
	if host.StatusOf(err) != 403 || !strings.Contains(err.Error(), "isn't shared") {
		t.Fatal(err)
	}
	if _, err := (&Host{}).Models(context.Background()); err == nil {
		t.Fatal("no address must be an error")
	}
}

func TestOpenModelStreamRequest(t *testing.T) {
	for protocol, path := range map[string]string{"openai-response": "/v1/responses", "claude": "/v1/messages", "openai": "/v1/chat/completions"} {
		h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || r.URL.Path != path || string(body) != `{"stream":true}` {
				t.Errorf("%s: %s %s %s", protocol, r.Method, r.URL.Path, body)
			}
			if r.Header.Get(accountHeader) != "codex/acct-1" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Forwarded-For") != "" {
				t.Errorf("%s: headers %v", protocol, r.Header)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1900000000")
			io.WriteString(w, "data: {\"a\":1}\n\n")
		})
		opened, err := h.OpenModelStream(context.Background(), host.ModelRequest{Account: "codex/acct-1", Protocol: protocol, Model: "codex/gpt-5.5", Body: []byte(`{"stream":true}`)})
		if err != nil || opened.ID == "" || opened.StatusCode != 200 || opened.Header.Get("Anthropic-Ratelimit-Unified-5h-Reset") != "1900000000" {
			t.Fatalf("%s: %+v %v", protocol, opened, err)
		}
		var body []byte
		for {
			chunk, err := h.ReadModelStream(opened.ID)
			if err != nil {
				t.Fatal(err)
			}
			body = append(body, chunk.Payload...)
			if chunk.Done {
				break
			}
		}
		if string(body) != "data: {\"a\":1}\n\n" {
			t.Fatalf("%s: body %q", protocol, body)
		}
		if err := h.CloseModelStream(opened.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.ReadModelStream(opened.ID); err == nil {
			t.Fatal("a closed stream must not be readable")
		}
	}
	h := &Host{Addr: "127.0.0.1:1"}
	if _, err := h.OpenModelStream(context.Background(), host.ModelRequest{Account: "a", Protocol: "gemini"}); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	if _, err := h.OpenModelStream(context.Background(), host.ModelRequest{Protocol: "claude"}); err == nil {
		t.Fatal("a request without an account must not be sent unpinned")
	}
}

// pinRestSample is Magpie's answer when the pinned account rests
// (fallback.go pinTo), in the Anthropic error format of /v1/messages.
const pinRestSample = `{"type":"error","error":{"type":"rate_limit_error","message":"X-Magpie-Account: a@example.com rests until 2026-11-21T10:05:00Z (quota); no other account is tried in its place"}}` + "\n"

func TestOpenModelStreamErrorStatus(t *testing.T) {
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, pinRestSample)
	})
	opened, err := h.OpenModelStream(context.Background(), host.ModelRequest{Account: "claude/x", Protocol: "claude", Body: []byte("{}")})
	var hostErr *host.Error
	if !errors.As(err, &hostErr) || hostErr.Status != 429 || opened.StatusCode != 429 || opened.ID != "" {
		t.Fatalf("%+v %v", opened, err)
	}
	if hostErr.Message != strings.TrimSpace(pinRestSample) || !strings.Contains(err.Error(), "rests until") {
		t.Fatalf("message %q", hostErr.Message)
	}
	long := gatewayError("x", 500, []byte(strings.Repeat("é", errorBodyLimit)))
	if msg := long.(*host.Error).Message; len(msg) > errorBodyLimit || !strings.HasPrefix(msg, "é") || strings.ContainsRune(msg, '\uFFFD') {
		t.Fatalf("truncated message %d bytes", len(msg))
	}
}

// blockingServer writes one event and holds the response open until the
// client goes away, which it reports on gone.
func blockingServer(t *testing.T) (*Host, chan struct{}) {
	gone := make(chan struct{})
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": keepalive\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	})
	return h, gone
}

func TestCloseModelStreamCancelsBlockedRead(t *testing.T) {
	h, gone := blockingServer(t)
	opened, err := h.OpenModelStream(context.Background(), host.ModelRequest{Account: "codex/a", Protocol: "openai-response", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if chunk, err := h.ReadModelStream(opened.ID); err != nil || string(chunk.Payload) != ": keepalive\n\n" {
		t.Fatalf("%q %v", chunk.Payload, err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := h.ReadModelStream(opened.ID)
		read <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := h.CloseModelStream(opened.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a cut read must be an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not unblock the read")
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel the upstream request")
	}
	if err := h.CloseModelStream(opened.ID); err != nil {
		t.Fatal("closing twice must be harmless")
	}
}

// ctx cancels a request that waits for its response headers.
func TestModelStreamContextCancelsBeforeHeaders(t *testing.T) {
	gone := make(chan struct{})
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client going away once the body is read.
		io.ReadAll(r.Body)
		<-r.Context().Done()
		close(gone)
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if opened, err := h.OpenModelStream(ctx, host.ModelRequest{Account: "codex/a", Protocol: "openai", Body: []byte("{}")}); err == nil || opened.StatusCode != 0 {
		t.Fatalf("%+v %v", opened, err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling ctx did not cancel the request")
	}
}

// headersSeen replaces the gateway client with one that reports when the
// response headers of a request have arrived.
func headersSeen(t *testing.T) <-chan struct{} {
	t.Helper()
	seen := make(chan struct{}, 1)
	old := gatewayClient
	gatewayClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := gatewayTransport.RoundTrip(r)
		if err == nil {
			seen <- struct{}{}
		}
		return resp, err
	})}
	t.Cleanup(func() { gatewayClient = old })
	return seen
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Once the headers have arrived, ctx no longer ends the request: the
// stream stays readable until CloseModelStream.
func TestModelStreamOutlivesContextAfterHeaders(t *testing.T) {
	more := make(chan struct{})
	gone := make(chan struct{})
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client going away once the body is read.
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-more:
			io.WriteString(w, "data: {\"a\":1}\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
		<-r.Context().Done()
		close(gone)
	})
	seen := headersSeen(t)
	ctx, cancel := context.WithCancel(context.Background())
	opened, err := h.OpenModelStream(ctx, host.ModelRequest{Account: "codex/a", Protocol: "openai", Body: []byte("{}")})
	if err != nil || opened.StatusCode != 200 {
		t.Fatalf("%+v %v", opened, err)
	}
	<-seen
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(more)
	if chunk, err := h.ReadModelStream(opened.ID); err != nil || string(chunk.Payload) != "data: {\"a\":1}\n\n" {
		t.Fatalf("after cancelling ctx: %q %v", chunk.Payload, err)
	}
	select {
	case <-gone:
		t.Fatal("cancelling ctx after the headers cut the request")
	default:
	}
	if err := h.CloseModelStream(opened.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel the request")
	}
}

// The provider's error status counts when ctx is cancelled while its body
// is read: the body is read within errorBodyWait regardless of ctx.
func TestModelStreamErrorBodyOutlivesContext(t *testing.T) {
	release := make(chan struct{})
	h := testHost(t, func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client going away once the body is read.
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`)
	})
	seen := headersSeen(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-seen
		cancel()
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	opened, err := h.OpenModelStream(ctx, host.ModelRequest{Account: "claude/x", Protocol: "claude", Body: []byte("{}")})
	if opened.StatusCode != 401 || host.StatusOf(err) != 401 || !strings.Contains(err.Error(), "OAuth token has expired") {
		t.Fatalf("%+v %v", opened, err)
	}

	// A body that never ends is cut after errorBodyWait; the status stays.
	defer func(was time.Duration) { errorBodyWait = was }(errorBodyWait)
	errorBodyWait = 100 * time.Millisecond
	stuck := make(chan struct{})
	defer close(stuck)
	h = testHost(t, func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client going away once the body is read.
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":`)
		w.(http.Flusher).Flush()
		select {
		case <-stuck:
		case <-r.Context().Done():
		}
	})
	started := time.Now()
	opened, err = h.OpenModelStream(context.Background(), host.ModelRequest{Account: "claude/x", Protocol: "claude", Body: []byte("{}")})
	if opened.StatusCode != 403 || host.StatusOf(err) != 403 || time.Since(started) > 5*time.Second {
		t.Fatalf("%+v %v after %s", opened, err, time.Since(started))
	}
}

func TestPinValue(t *testing.T) {
	if got := pinValue("codex/abc-123"); got != "codex/abc-123" {
		t.Fatal(got)
	}
	if got := pinValue("claude/北冥"); got != "claude%2F%E5%8C%97%E5%86%A5" {
		t.Fatal(got)
	}
}
