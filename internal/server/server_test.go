package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
)

func newServer(t *testing.T) (*Server, *config.Store) {
	t.Helper()
	return newServerWith(t, "")
}

// newServerWith starts a server on a data directory whose config.json
// holds file, or on one without config.json when file is empty.
func newServerWith(t *testing.T, file string) (*Server, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	if file != "" {
		if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, cfg, err := config.Open(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.Options{Host: &magpie.Host{}, Version: "test", DataDir: dir})
	s := New(store, cfg, nil, e, "test")
	t.Cleanup(s.Shutdown)
	return s, store
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8317"
	h.ServeHTTP(rec, req)
	var out map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, out
}

func TestConfigPatchIsSavedAndApplied(t *testing.T) {
	s, store := newServer(t)
	h := s.Handler()
	code, _ := do(t, h, http.MethodPatch, "/api/config", `{"config_token":"t1","mcp_key":"secret","bark_url":"https://b/k"}`)
	if code != http.StatusOK || string(store.Raw()["mcp_key"]) != `"secret"` {
		t.Fatalf("patch: %d %v", code, store.Raw())
	}
	_, status := do(t, h, http.MethodGet, "/api/status", "")
	cfg := status["config"].(map[string]any)
	if cfg["config_token"] != "t1" || cfg["mcp_key_set"] != true || cfg["mcp_key"] != nil || cfg["bark_url"] != "已设置" {
		t.Fatalf("status config %v", cfg)
	}
	if code, _ := do(t, h, http.MethodPatch, "/api/config", `{"ignition":[`); code != http.StatusBadRequest {
		t.Fatalf("an invalid body must be refused, got %d", code)
	}
}

func TestPageStateRoundTrip(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	body := `{"config_token":"t1","chart_view":{"window":"7d","sort":"low","ranges":{"7d":"15d"}},"chart_expanded":["Claude"],` +
		`"mcp_public_url":"https://mcp.example.com","mask_accounts":true,"events_show_5h_resets":true}`
	if code, _ := do(t, h, http.MethodPatch, "/api/config", body); code != http.StatusOK {
		t.Fatalf("patch: %d", code)
	}
	_, raw := do(t, h, http.MethodGet, "/api/config", "")
	view, _ := raw["chart_view"].(map[string]any)
	if view == nil || view["window"] != "7d" || fmt.Sprint(raw["chart_expanded"]) != "[Claude]" ||
		raw["mcp_public_url"] != "https://mcp.example.com" || raw["mask_accounts"] != true || raw["events_show_5h_resets"] != true {
		t.Fatalf("config %v", raw)
	}
	body = `{"config_token":"t2","chart_view":null,"chart_expanded":null,"mcp_public_url":null,"mask_accounts":null,"events_show_5h_resets":null}`
	if code, _ := do(t, h, http.MethodPatch, "/api/config", body); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	_, raw = do(t, h, http.MethodGet, "/api/config", "")
	if len(raw) != 1 || raw["config_token"] != "t2" {
		t.Fatalf("deleted keys remain: %v", raw)
	}
}

func TestPageAndUnknownRoutes(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:8317"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("page: %d", rec.Code)
	}
	if code, _ := do(t, h, http.MethodGet, "/api/missing", ""); code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", code)
	}
}

func TestToolCallBeforeStart(t *testing.T) {
	s, _ := newServer(t)
	code, out := do(t, s.Handler(), http.MethodPost, "/api/tools/list_quotas", `{}`)
	result, _ := out["result"].(map[string]any)
	if code != http.StatusOK || result["ready"] != false || result["state"] != "starting" {
		t.Fatalf("tool call: %d %v", code, out)
	}
	if _, out := do(t, s.Handler(), http.MethodPost, "/api/tools/unknown", `{}`); out["error"] == nil {
		t.Fatalf("unknown tool: %v", out)
	}
}

func TestPatchRefusesSameKeys(t *testing.T) {
	s, store := newServer(t)
	h := s.Handler()
	if code, _ := do(t, h, http.MethodPatch, "/api/config", `{"admin_key":"lladm_x"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	for _, patch := range []string{
		`{"mcp_key":"lladm_x"}`,
		`{"mcp_key":"  lladm_x\t"}`,
		`{"admin_key":"same","mcp_key":"same","mcp_enabled":true}`,
	} {
		code, out := do(t, h, http.MethodPatch, "/api/config", patch)
		message, _ := out["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(message, "admin_key") || !strings.Contains(message, "mcp_key") ||
			strings.Contains(message, "lladm_x") || strings.Contains(message, "same") {
			t.Fatalf("%s: %d %q", patch, code, message)
		}
	}
	if raw := store.Raw(); len(raw) != 1 || string(raw["admin_key"]) != `"lladm_x"` {
		t.Fatalf("saved %v", raw)
	}
}

// A file with the same admin_key and mcp_key, written by hand, starts
// with the public listener serving neither part.
func TestStartupWithSameKeys(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	addr := freeAddr(t)
	s, _ := newServerWith(t, fmt.Sprintf(`{"public_listen":%q,"web_enabled":true,"admin_key":" k1 ","mcp_enabled":true,"mcp_key":"k1"}`, addr))
	_, status := do(t, s.Handler(), http.MethodGet, "/api/status", "")
	public := status["public"].(map[string]any)
	message, _ := public["error"].(string)
	if public["running"] != false || public["web"] != false || public["mcp"] != false ||
		!strings.Contains(message, "admin_key") || strings.Contains(message, "k1") {
		t.Fatalf("public %v", public)
	}
	if mcpState := status["mcp"].(map[string]any); mcpState["error"] != message {
		t.Fatalf("mcp %v", mcpState)
	}
	if !strings.Contains(logged.String(), "admin_key") || strings.Contains(logged.String(), "k1") {
		t.Fatalf("log %q", logged.String())
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).Dial("tcp", addr); err == nil {
		conn.Close()
		t.Fatal("the public listener is up")
	}
	// Changing one key opens both parts.
	if code, _ := do(t, s.Handler(), http.MethodPatch, "/api/config", `{"mcp_key":"k2"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	_, status = do(t, s.Handler(), http.MethodGet, "/api/status", "")
	if public := status["public"].(map[string]any); public["web"] != true || public["mcp"] != true || public["error"] != "" {
		t.Fatalf("after the fix %v", public)
	}
}

func TestPatchRefusesInvalidTimezone(t *testing.T) {
	s, store := newServer(t)
	// "Local" is the process time zone to time.LoadLocation; an empty
	// timezone means that.
	for _, name := range []string{"Asia/Nowhere", "Local"} {
		code, out := do(t, s.Handler(), http.MethodPatch, "/api/config", `{"timezone":"`+name+`"}`)
		if message, _ := out["error"].(string); code != http.StatusBadRequest || !strings.Contains(message, "timezone") || strings.Contains(message, name) {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		if len(store.Raw()) != 0 {
			t.Fatalf("%s: saved %v", name, store.Raw())
		}
	}
	for _, name := range []string{"Asia/Shanghai", "UTC"} {
		if code, _ := do(t, s.Handler(), http.MethodPatch, "/api/config", `{"timezone":"`+name+`"}`); code != http.StatusOK {
			t.Fatal(name, code)
		}
	}
	if code, _ := do(t, s.Handler(), http.MethodPatch, "/api/config", `{"timezone":""}`); code != http.StatusOK {
		t.Fatal(code)
	}
}

// Two PATCH requests that interleave between saving and applying would
// leave the running configuration on the one saved first. The hook holds
// the first request there while the second one is sent.
func TestConcurrentPatchesApplyInSaveOrder(t *testing.T) {
	s, store := newServer(t)
	h := s.Handler()
	var calls atomic.Int32
	firstSaved := make(chan struct{})
	secondSent := make(chan struct{})
	s.patched = func() {
		if calls.Add(1) != 1 {
			return
		}
		close(firstSaved)
		<-secondSent
		// Give the second request time to save and apply if it can.
		time.Sleep(200 * time.Millisecond)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		do(t, h, http.MethodPatch, "/api/config", `{"config_token":"first"}`)
	}()
	<-firstSaved
	go func() {
		defer wg.Done()
		close(secondSent)
		do(t, h, http.MethodPatch, "/api/config", `{"config_token":"second"}`)
	}()
	wg.Wait()
	_, status := do(t, h, http.MethodGet, "/api/status", "")
	running := status["config"].(map[string]any)["config_token"]
	saved := string(store.Raw()["config_token"])
	if saved != `"second"` || running != "second" {
		t.Fatalf("file %s, running %v", saved, running)
	}
}
