package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Code != http.StatusForbidden {
		t.Fatalf("foreign preflight %d %v", rec.Code, rec.Header())
	}

	cases := []struct {
		name    string
		headers map[string]string
		code    int
		cors    bool
	}{
		{"null origin with the token", map[string]string{"Origin": "null", pageHeader: s.pageToken}, http.StatusOK, true},
		{"null origin with a wrong token", map[string]string{"Origin": "null", pageHeader: s.pageToken + "x"}, http.StatusForbidden, false},
		{"null origin without the token", map[string]string{"Origin": "null"}, http.StatusForbidden, false},
		{"another origin with the token", map[string]string{"Origin": "https://evil.example", pageHeader: s.pageToken}, http.StatusForbidden, false},
		{"same origin with the token", map[string]string{"Origin": "http://" + host, pageHeader: s.pageToken}, http.StatusOK, false},
		{"no origin", nil, http.StatusOK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/status", "/api/config"} {
				rec := internalRequest(h, http.MethodGet, path, host, tc.headers)
				if rec.Code != tc.code {
					t.Fatalf("%s: %d, want %d", path, rec.Code, tc.code)
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

// recorder is a handler that counts the requests that reach it.
type recorder struct{ calls []string }

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.calls = append(r.calls, req.Method+" "+req.URL.Path)
	w.WriteHeader(http.StatusOK)
}

// A browser request to /api/ runs its route only with the page token and
// from the plugin page's origin. The response of a refused request does
// not matter: a cross-site page could not read it anyway, and the route,
// with its side effects, must not run.
func TestBrowserRequestsNeedThePage(t *testing.T) {
	const token = "page-token"
	host := "127.0.0.1:8317"
	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/refresh"},
		{http.MethodPost, "/api/test-notify"},
		{http.MethodPost, "/api/ignite"},
		{http.MethodPatch, "/api/config"},
		{http.MethodGet, "/api/config"},
		{http.MethodPost, "/api/tools/refresh_quotas"},
	}
	refused := []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "https://evil.example", pageHeader: token},
		{"Origin": "null"},
		{"Origin": "null", pageHeader: "wrong"},
		{"Origin": "http://127.0.0.1:9999", pageHeader: token},
		{"Origin": "https://" + host, pageHeader: token},
		{"Origin": "http://" + host},
		{"Sec-Fetch-Site": "cross-site", pageHeader: token},
		{"Sec-Fetch-Site": "same-site", pageHeader: token},
		{"Sec-Fetch-Site": "none", pageHeader: token},
		{"Sec-Fetch-Site": "same-origin"},
		{"Sec-Fetch-Site": "same-origin", "Origin": "https://evil.example", pageHeader: token},
	}
	allowed := []map[string]string{
		nil, // the plugin host
		{"Origin": "null", pageHeader: token},
		{"Origin": "null", "Sec-Fetch-Site": "cross-site", pageHeader: token},
		{"Origin": "http://" + host, pageHeader: token},
		{"Origin": "http://LOCALHOST:8317", pageHeader: token, "Host": "localhost:8317"},
		{"Sec-Fetch-Site": "same-origin", pageHeader: token},
	}
	send := func(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Host = host
		for name, value := range headers {
			if name == "Host" {
				req.Host = value
				continue
			}
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, route := range routes {
		for _, headers := range refused {
			next := &recorder{}
			rec := send(guardInternal(next, token), route.method, route.path, headers)
			if rec.Code != http.StatusForbidden || len(next.calls) != 0 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("%s %s %v: %d, routes ran %v", route.method, route.path, headers, rec.Code, next.calls)
			}
		}
		for _, headers := range allowed {
			next := &recorder{}
			if rec := send(guardInternal(next, token), route.method, route.path, headers); rec.Code != http.StatusOK || len(next.calls) != 1 {
				t.Fatalf("%s %s %v: %d", route.method, route.path, headers, rec.Code)
			}
		}
	}
	// The page itself opens in a browser tab from anywhere.
	next := &recorder{}
	if rec := send(guardInternal(next, token), http.MethodGet, "/", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); rec.Code != http.StatusOK {
		t.Fatalf("page %d", rec.Code)
	}
	// A preflight runs no route.
	next = &recorder{}
	if rec := send(guardInternal(next, token), http.MethodOptions, "/api/config", map[string]string{"Origin": "null"}); rec.Code != http.StatusNoContent || len(next.calls) != 0 {
		t.Fatalf("preflight %d %v", rec.Code, next.calls)
	}
	// Without a token no browser request runs.
	next = &recorder{}
	if rec := send(guardInternal(next, ""), http.MethodPost, "/api/refresh", map[string]string{"Origin": "null", pageHeader: ""}); rec.Code != http.StatusForbidden || len(next.calls) != 0 {
		t.Fatalf("empty token %d", rec.Code)
	}
}

// A cross-site PATCH on the real server changes nothing.
func TestCrossSitePatchIsRefused(t *testing.T) {
	s, store := newServer(t)
	for _, headers := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "null"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"mcp_enabled":true,"mcp_key":"stolen"}`))
		req.Host = "127.0.0.1:8317"
		req.Header.Set("Content-Type", "text/plain")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || len(store.Raw()) != 0 {
			t.Fatalf("%v: %d, config %v", headers, rec.Code, store.Raw())
		}
	}
	_, status := do(t, s.Handler(), http.MethodGet, "/api/status", "")
	if cfg := status["config"].(map[string]any); cfg["mcp_enabled"] != false {
		t.Fatalf("applied: %v", cfg)
	}
}

// The page opened in a browser tab on the internal listener, and in
// Magpie's sandboxed frame, reaches the API with the token it was served.
func TestPageReachesTheAPIWithItsToken(t *testing.T) {
	s, store := newServer(t)
	h := s.Handler()
	host := "127.0.0.1:8317"
	page := internalRequest(h, http.MethodGet, "/", host, map[string]string{"Sec-Fetch-Site": "none"})
	m := regexp.MustCompile(`<meta name="lamplighter-page" content="([^"]*)">`).FindSubmatch(page.Body.Bytes())
	if page.Code != http.StatusOK || m == nil {
		t.Fatalf("page %d without a token", page.Code)
	}
	token := html.UnescapeString(string(m[1]))
	for _, c := range []struct {
		name    string
		headers map[string]string
	}{
		{"tab", map[string]string{"Origin": "http://" + host, "Sec-Fetch-Site": "same-origin"}},
		{"frame", map[string]string{"Origin": "null", "Sec-Fetch-Site": "cross-site"}},
	} {
		get := map[string]string{pageHeader: token, "Sec-Fetch-Site": c.headers["Sec-Fetch-Site"]}
		if c.name == "frame" {
			get["Origin"] = "null"
		}
		if rec := internalRequest(h, http.MethodGet, "/api/status", host, get); rec.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/status %d %s", c.name, rec.Code, rec.Body)
		}
		req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config_token":"`+c.name+`","bark_url":"https://b/`+c.name+`"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(pageHeader, token)
		for name, value := range c.headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || string(store.Raw()["bark_url"]) != `"https://b/`+c.name+`"` {
			t.Fatalf("%s: PATCH /api/config %d %s, config %v", c.name, rec.Code, rec.Body, store.Raw())
		}
	}
}
