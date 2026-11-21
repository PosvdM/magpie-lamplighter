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

func TestModelStreamContextCancels(t *testing.T) {
	h, gone := blockingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	opened, err := h.OpenModelStream(ctx, host.ModelRequest{Account: "codex/a", Protocol: "openai", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	defer h.CloseModelStream(opened.ID)
	cancel()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling ctx did not cancel the request")
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
