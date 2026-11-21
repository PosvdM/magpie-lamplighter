package mcp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// leaky is an error text with an email, a URL query and a token, and
// leaked its parts that no answer may show.
const leaky = "carol.secret@corp.example at https://vendor.example/v1?user=carol%40corp.example: Bearer ya29.A0b1C2d3E4f5G6h7I8j9K0l1M2n3O4p5Q6r7S8t9"

var leaked = []string{"carol.secret", "corp.example", "user=", "A0b1C2d3E4f5G6h7I8j9"}

// failingBackend fails every tool call with leaky.
type failingBackend struct{}

func (failingBackend) CallTool(context.Context, string, map[string]string) (any, error) {
	return nil, errors.New(leaky)
}

// The error of a tool call and of the listener leave the server redacted.
func TestErrorsAreRedacted(t *testing.T) {
	s := New(failingBackend{}, "test")
	s.key = "test-key"
	defer s.Close()
	server := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	status, raw := post(t, client, server.URL+"/mcp", "POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_quotas","arguments":{}}}`, "test-key", "", "2025-06-18")
	if status != 200 || !bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("tools/call: %d %s", status, raw)
	}
	s.mu.Lock()
	s.listenErr = "Public listener stopped: " + leaky
	s.state.Enabled = true
	s.mu.Unlock()
	for _, text := range []string{string(raw), s.Public().Error, s.State().Error} {
		for _, part := range leaked {
			if strings.Contains(text, part) {
				t.Errorf("%q shows %q", text, part)
			}
		}
	}
}
