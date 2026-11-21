package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func request(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestPublicListenerFollowsConfig(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	addr := freeAddr(t)

	_, status := do(t, h, http.MethodGet, "/api/status", "")
	public := status["public"].(map[string]any)
	if public["running"] != false || public["web"] != false || public["mcp"] != false || public["error"] != "" {
		t.Fatalf("default public %v", public)
	}
	mcpState := status["mcp"].(map[string]any)
	for _, field := range []string{"enabled", "listen", "running", "error", "path"} {
		if _, ok := mcpState[field]; !ok {
			t.Fatalf("status.mcp lacks %s: %v", field, mcpState)
		}
	}

	// web_enabled without an admin key serves nothing.
	patch := fmt.Sprintf(`{"public_listen":%q,"web_enabled":true}`, addr)
	if code, _ := do(t, h, http.MethodPatch, "/api/config", patch); code != http.StatusOK {
		t.Fatal(code)
	}
	_, status = do(t, h, http.MethodGet, "/api/status", "")
	if public := status["public"].(map[string]any); public["running"] != false || public["web"] != false {
		t.Fatalf("web without key %v", public)
	}

	if code, _ := do(t, h, http.MethodPatch, "/api/config", `{"admin_key":"lladm_secret"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	_, status = do(t, h, http.MethodGet, "/api/status", "")
	public = status["public"].(map[string]any)
	if public["running"] != true || public["web"] != true || public["mcp"] != false || public["listen"] != addr {
		t.Fatalf("web on %v", public)
	}
	cfg := status["config"].(map[string]any)
	if cfg["admin_key_set"] != true || cfg["admin_key"] != nil {
		t.Fatalf("status leaks the admin key: %v", cfg)
	}

	base := "http://" + addr
	if code, _ := request(t, http.MethodGet, base+"/api/status", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("public API without key: %d", code)
	}
	if code, _ := request(t, http.MethodGet, base+"/api/status", "wrong", ""); code != http.StatusUnauthorized {
		t.Fatalf("public API with a wrong key: %d", code)
	}
	code, out := request(t, http.MethodGet, base+"/api/status", "lladm_secret", "")
	if code != http.StatusOK || out["public"] == nil {
		t.Fatalf("public API with key: %d %v", code, out)
	}
	// The internal listener still takes no key.
	if code, _ := do(t, h, http.MethodGet, "/api/config", ""); code != http.StatusOK {
		t.Fatalf("internal API: %d", code)
	}

	// Rotating the key through the public API answers the request and
	// keeps the listener.
	code, _ = request(t, http.MethodPatch, base+"/api/config", "lladm_secret", `{"admin_key":"lladm_next","mcp_enabled":true,"mcp_key":"llmcp_k"}`)
	if code != http.StatusOK {
		t.Fatalf("rotate: %d", code)
	}
	if code, _ := request(t, http.MethodGet, base+"/api/status", "lladm_secret", ""); code != http.StatusUnauthorized {
		t.Fatalf("old key: %d", code)
	}
	code, out = request(t, http.MethodGet, base+"/api/status", "lladm_next", "")
	if public := out["public"].(map[string]any); code != http.StatusOK || public["web"] != true || public["mcp"] != true {
		t.Fatalf("new key: %d %v", code, out)
	}

	if code, _ := do(t, h, http.MethodPatch, "/api/config", `{"web_enabled":false,"mcp_enabled":false}`); code != http.StatusOK {
		t.Fatal(code)
	}
	_, status = do(t, h, http.MethodGet, "/api/status", "")
	if public := status["public"].(map[string]any); public["running"] != false || public["web"] != false || public["mcp"] != false {
		t.Fatalf("both off %v", public)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is still bound: %v", err)
	}
	ln.Close()
}

// A page or API request on the public listener waits for the server's
// lock; Shutdown must not hold it while it waits for that request.
func TestShutdownWithPublicRequest(t *testing.T) {
	s, _ := newServer(t)
	addr := freeAddr(t)
	patch := fmt.Sprintf(`{"public_listen":%q,"web_enabled":true,"admin_key":"k"}`, addr)
	if code, _ := do(t, s.Handler(), http.MethodPatch, "/api/config", patch); code != http.StatusOK {
		t.Fatal(code)
	}
	// Shutdown queues for the lock first, then the request does, so
	// Shutdown takes the lock while the request waits for it.
	s.mu.Lock()
	stopped := make(chan struct{})
	go func() { s.Shutdown(); close(stopped) }()
	time.Sleep(50 * time.Millisecond)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/status", nil)
		req.Header.Set("Authorization", "Bearer k")
		client := &http.Client{Timeout: 10 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the request reach status()
	s.mu.Unlock()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown deadlocked with a public request")
	}
	<-sent
}

// The PATCH that moves the public listener is answered on the connection
// it came on, and the listener comes up at the new address.
func TestPublicPatchMovesListener(t *testing.T) {
	s, store := newServer(t)
	addr, moved := freeAddr(t), freeAddr(t)
	patch := fmt.Sprintf(`{"public_listen":%q,"web_enabled":true,"admin_key":"k"}`, addr)
	if code, _ := do(t, s.Handler(), http.MethodPatch, "/api/config", patch); code != http.StatusOK {
		t.Fatal(code)
	}
	code, out := request(t, http.MethodPatch, "http://"+addr+"/api/config", "k", fmt.Sprintf(`{"public_listen":%q}`, moved))
	if code != http.StatusOK || out["public_listen"] != moved {
		t.Fatalf("move: %d %v", code, out)
	}
	if string(store.Raw()["public_listen"]) != fmt.Sprintf("%q", moved) {
		t.Fatalf("saved %v", store.Raw())
	}
	if code, _ := request(t, http.MethodGet, "http://"+moved+"/api/status", "k", ""); code != http.StatusOK {
		t.Fatalf("new address: %d", code)
	}
}
