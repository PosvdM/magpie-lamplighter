package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/web"
)

// internalRequest sends a request to the internal handler with host and
// the given headers.
func internalRequest(h http.Handler, method, path, host string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Host = host
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPageToken(t *testing.T) {
	s, _ := newServer(t)
	raw, err := base64.RawURLEncoding.DecodeString(s.pageToken)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q: %v", s.pageToken, err)
	}
	if other := newPageToken(); other == s.pageToken {
		t.Fatal("tokens repeat")
	}
	rec := internalRequest(s.Handler(), http.MethodGet, "/", "127.0.0.1:8317", map[string]string{"Origin": "null"})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), web.PageWith(s.pageToken)) {
		t.Fatalf("page %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("the page must not carry CORS headers")
	}
}

func TestInternalCORS(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	host := "127.0.0.1:8317"

	rec := internalRequest(h, http.MethodOptions, "/api/config", host, map[string]string{
		"Origin":                         "null",
		"Access-Control-Request-Method":  "PATCH",
		"Access-Control-Request-Headers": "content-type, x-lamplighter-page",
	})
	header := rec.Header()
	if rec.Code != http.StatusNoContent ||
		header.Get("Access-Control-Allow-Origin") != "null" ||
		header.Get("Access-Control-Allow-Methods") != "GET, POST, PATCH" ||
		header.Get("Access-Control-Allow-Headers") != "Content-Type, Authorization, X-Lamplighter-Page" ||
		header.Get("Access-Control-Max-Age") != "600" ||
		header.Get("Vary") != "Origin" ||
		header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight %d %v", rec.Code, header)
	}
	// A preflight from a real origin gets no CORS answer.
	rec = internalRequest(h, http.MethodOptions, "/api/config", host, map[string]string{"Origin": "https://evil.example"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Code == http.StatusNoContent {
		t.Fatalf("foreign preflight %d %v", rec.Code, rec.Header())
	}

	cases := []struct {
		name    string
		headers map[string]string
		cors    bool
	}{
		{"null origin with the token", map[string]string{"Origin": "null", pageHeader: s.pageToken}, true},
		{"null origin with a wrong token", map[string]string{"Origin": "null", pageHeader: s.pageToken + "x"}, false},
		{"null origin without the token", map[string]string{"Origin": "null"}, false},
		{"another origin with the token", map[string]string{"Origin": "https://evil.example", pageHeader: s.pageToken}, false},
		{"no origin", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/status", "/api/config"} {
				rec := internalRequest(h, http.MethodGet, path, host, tc.headers)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d; requests are served with or without CORS", path, rec.Code)
				}
				allow, vary := rec.Header().Get("Access-Control-Allow-Origin"), rec.Header().Get("Vary")
				if tc.cors && (allow != "null" || vary != "Origin") || !tc.cors && (allow != "" || vary != "") {
					t.Fatalf("%s: %v", path, rec.Header())
				}
				if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Fatalf("%s: credentials allowed", path)
				}
			}
		})
	}
	// The plugin host calls the tools without an Origin.
	rec = internalRequest(h, http.MethodPost, "/api/tools/list_quotas", host, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("tool call %d %v", rec.Code, rec.Header())
	}
}

func TestInternalHostCheck(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	for _, host := range []string{"127.0.0.1:8317", "127.0.0.1", "localhost:8317", "LOCALHOST", "[::1]:8317", "[::1]", "127.0.0.2:1"} {
		for _, path := range []string{"/", "/api/status"} {
			if rec := internalRequest(h, http.MethodGet, path, host, nil); rec.Code != http.StatusOK {
				t.Fatalf("%s %s: %d", host, path, rec.Code)
			}
		}
	}
	for _, host := range []string{"", "evil.example", "evil.example:8317", "localhost.evil.example:8317", "127.0.0.1.evil.example", "192.168.1.2:8317", "0.0.0.0:8317", "[::]:8317"} {
		for _, path := range []string{"/", "/api/status", "/api/config", "/api/missing"} {
			if rec := internalRequest(h, http.MethodGet, path, host, nil); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "<html") {
				t.Fatalf("%q %s: %d", host, path, rec.Code)
			}
		}
		rec := internalRequest(h, http.MethodOptions, "/api/config", host, map[string]string{"Origin": "null"})
		if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%q preflight: %d", host, rec.Code)
		}
		rec = internalRequest(h, http.MethodPost, "/api/tools/list_quotas", host, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%q tool call: %d", host, rec.Code)
		}
	}
}

// The public listener has neither the page token nor CORS nor the Host
// check: a browser opens it at the domain in front of it.
func TestPublicListenerHasNoCORS(t *testing.T) {
	s, _ := newServer(t)
	addr := freeAddr(t)
	patch := fmt.Sprintf(`{"public_listen":%q,"web_enabled":true,"admin_key":"k"}`, addr)
	if code, _ := do(t, s.Handler(), http.MethodPatch, "/api/config", patch); code != http.StatusOK {
		t.Fatal(code)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	send := func(method, path string, headers map[string]string) (*http.Response, []byte) {
		req, err := http.NewRequest(method, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "lamplighter.example.com"
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	resp, body := send(http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, web.Page()) || strings.Contains(string(body), s.pageToken) {
		t.Fatalf("public page %d", resp.StatusCode)
	}
	resp, _ = send(http.MethodGet, "/api/status", map[string]string{"Authorization": "Bearer k", "Origin": "null", pageHeader: s.pageToken})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("public API %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = send(http.MethodOptions, "/api/status", map[string]string{"Origin": "null"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("public preflight %d %v", resp.StatusCode, resp.Header)
	}
}
