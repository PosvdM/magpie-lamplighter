package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
)

func newServer(t *testing.T) (*Server, *config.Store) {
	t.Helper()
	dir := t.TempDir()
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
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
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

func TestPageAndUnknownRoutes(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
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
