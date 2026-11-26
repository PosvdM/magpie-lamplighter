// Package server serves the page, the HTTP API and the tool calls of the
// Magpie plugin host, and applies configuration changes.
//
// The internal listener runs on loopback and takes no key: Magpie reaches
// it for the plugin page (magpie.page.service) and for the tools of the
// plugin host. It refuses a Host that is not loopback, and an API request
// from a browser without the page token of the plugin page; see
// guardInternal. The
// public listener at public_listen, run by package mcp,
// serves the same handler behind admin_key when web_enabled, and /mcp
// behind mcp_key when mcp_enabled; see docs/architecture.md.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/mcp"
	"github.com/PosvdM/magpie-lamplighter/internal/redact"
	"github.com/PosvdM/magpie-lamplighter/internal/web"
)

const (
	stopTimeout = 10 * time.Second
	maxBody     = 1 << 20
	// toolTimeout keeps a tool call under the 55 seconds the plugin host
	// gives it.
	toolTimeout = 50 * time.Second
)

// Server ties the configuration store, the engine and the MCP server
// together.
type Server struct {
	store  *config.Store
	engine *engine.Engine
	mcp    *mcp.Server
	tools  *engine.MCPBackend
	// handler serves the internal listener: the routes with the page
	// token in the page, behind the Host check and with CORS for the
	// page token (guardInternal). The public listener serves the routes
	// without them, behind the admin key.
	handler   http.Handler
	pageToken string // random per process; see guardInternal

	mu      sync.Mutex
	stopped bool

	// patchMu serializes PATCH /api/config from the merge to the response,
	// so that the configurations are applied in the order they are saved.
	// Shutdown takes it after setting stopped, to wait for a write in
	// progress; see patchConfig.
	patchMu sync.Mutex
	// patched, when set by a test, runs between saving a patch and applying
	// it.
	patched func()
}

// errSameKeysPublic is the public listener's error while admin_key and
// mcp_key are the same.
const errSameKeysPublic = "admin_key 与 mcp_key 相同，对外监听不提供页面、接口和 /mcp；请更换其中一个密钥"

// New creates the server and applies cfg, the configuration read from
// store, or parseErr when it could not be read.
func New(store *config.Store, cfg config.Config, parseErr error, e *engine.Engine, version string) *Server {
	backend := engine.NewMCPBackend(e)
	s := &Server{store: store, engine: e, mcp: mcp.New(backend, version), tools: backend}
	s.pageToken = newPageToken()
	s.handler = guardInternal(s.routes(web.PageWith(s.pageToken)), s.pageToken)
	s.mcp.SetWeb(s.routes(web.Page()))
	s.apply(cfg, parseErr)
	return s
}

// apply hands cfg to the engine and the public listener, which is rebound
// only when its address changes or it starts or stops serving.
//
// PATCH /api/config refuses the same admin_key and mcp_key, but the file
// read at startup may hold them: one key would then open both the API and
// /mcp, so the public listener serves neither.
func (s *Server) apply(cfg config.Config, parseErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	revision := s.engine.Configure(cfg, parseErr)
	if parseErr == nil {
		refused := ""
		if cfg.SameKeys() {
			refused = errSameKeysPublic
			log.Printf("error: %v; the public listener serves neither the page and API nor /mcp", config.ErrSameKeys)
		}
		s.mcp.Configure(mcp.Config{
			Listen:     cfg.PublicListen,
			MCPEnabled: cfg.MCPEnabled,
			MCPKey:     cfg.MCPKey,
			WebEnabled: cfg.WebEnabled,
			AdminKey:   cfg.AdminKey,
			Refused:    refused,
		})
		s.engine.MCPConfigured(revision)
	}
}

// Start starts the engine.
func (s *Server) Start() { s.engine.Start() }

// errStopped is the answer to PATCH /api/config once Shutdown has begun:
// the configuration is not saved.
var errStopped = errors.New("Lamplighter 正在退出，设置未保存")

// Shutdown stops the server for the process to exit. In order:
//
//  1. It marks the server stopped: configurations are no longer applied
//     and PATCH /api/config answers 503 without saving.
//  2. It cancels the engine: management operations return at once, a
//     queued one is dropped, and the one in progress is canceled.
//  3. It closes the public listener and waits for its requests, which
//     no longer wait for the engine.
//  4. It waits for a configuration write already in progress.
//  5. It waits up to stopTimeout for the background loop.
//
// When it returns, config.json is not written again, and the caller may
// release the instance lock once its own listener is closed. It does not
// hold mu while waiting: a page or API request may be waiting for mu.
func (s *Server) Shutdown() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	s.engine.Cancel()
	s.mcp.Close()
	// Wait for the write in progress; later ones see stopped.
	s.patchMu.Lock()
	s.patchMu.Unlock()
	s.engine.Stop(stopTimeout)
}

// isStopped reports whether Shutdown has begun.
func (s *Server) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// statusResponse is the status API: the engine's status, the state of
// /mcp in mcp and that of the public listener in public.
type statusResponse struct {
	engine.Status
	Public mcp.Public `json:"public"`
}

func (s *Server) status() statusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := statusResponse{Status: s.engine.Status(), Public: s.mcp.Public()}
	status.MCP = s.mcp.State()
	return status
}

// Handler serves the internal listener.
func (s *Server) Handler() http.Handler { return s.handler }

// routes returns the routes of both listeners, serving page at /.
func (s *Server) routes(page []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.status())
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		resp, err := s.engine.HistoryRange(r.Context(), r.URL.Query().Get("range"), r.URL.Query().Get("window"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		s.action(w, s.engine.Refresh(r.Context(), strings.TrimSpace(body.AuthIndex)))
	})
	mux.HandleFunc("POST /api/ignite", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Target string `json:"target"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Target) == "" {
			writeError(w, http.StatusBadRequest, errors.New("缺少 target"))
			return
		}
		s.action(w, s.engine.Ignite(r.Context(), strings.TrimSpace(body.Target)))
	})
	mux.HandleFunc("POST /api/test-notify", func(w http.ResponseWriter, r *http.Request) {
		results, err := s.engine.TestNotify(r.Context())
		if err != nil {
			s.action(w, err)
			return
		}
		ok := true
		for _, result := range results {
			ok = ok && result.OK
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "results": results, "status": s.status()})
	})
	mux.HandleFunc("POST /api/language", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Language string `json:"language"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		s.action(w, s.engine.SetLanguage(r.Context(), body.Language))
	})
	mux.HandleFunc("POST /api/check-update", func(w http.ResponseWriter, r *http.Request) {
		s.action(w, s.engine.CheckRelease())
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.store.Raw())
	})
	mux.HandleFunc("PATCH /api/config", func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]json.RawMessage
		if !readJSON(w, r, &patch) {
			return
		}
		raw, err := s.patchConfig(patch)
		if errors.Is(err, errStopped) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, raw)
	})
	mux.HandleFunc("POST /api/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		var args map[string]string
		if !readJSON(w, r, &args) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), toolTimeout)
		defer cancel()
		result, err := s.tools.CallTool(ctx, r.PathValue("name"), args)
		if err != nil {
			writeToolError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": result})
	})
	return mux
}

// patchConfig saves patch, applies the result and returns the saved keys.
// A patch of only the page's own state (config.IsPageState) is saved
// without being applied: the engine and the public listener are not
// configured again, and config_token stays the one of the last applied
// save.
// Two patches never interleave: otherwise the later one could be saved
// last and applied first, leaving the running configuration different
// from the file.
//
// Once Shutdown has begun it returns errStopped without saving: the
// instance lock is released after Shutdown, and a write after that could
// overwrite the file of the next process. The check is made under
// patchMu, which Shutdown takes after setting stopped, so every write
// either finishes before Shutdown returns or does not start.
func (s *Server) patchConfig(patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	s.patchMu.Lock()
	defer s.patchMu.Unlock()
	if s.isStopped() {
		return nil, errStopped
	}
	cfg, err := s.store.Patch(patch)
	if err != nil {
		return nil, err
	}
	if s.patched != nil {
		s.patched()
	}
	// The page's own state is only saved: applying it would wake the
	// background loop, which runs the ignitions due within its lead early,
	// and move config_applied_token, which the page's test notification
	// waits for.
	if config.IsPageState(patch) && s.applyPageState(cfg) {
		return s.store.Raw(), nil
	}
	s.apply(cfg, nil)
	return s.store.Raw(), nil
}

// applyPageState hands the page state of cfg to the engine for the status;
// see engine.ConfigurePageState.
func (s *Server) applyPageState(cfg config.Config) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped || s.engine.ConfigurePageState(cfg)
}

func (s *Server) action(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrNotRunning) || errors.Is(err, engine.ErrStopped) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if errors.Is(err, engine.ErrIgnitionPending) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": true, "status": s.status()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": redact.Text(err.Error()), "status": s.status()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.status()})
}

// readJSON decodes an optional JSON body into out. An empty body leaves out
// as it is.
func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err == nil && len(raw) > maxBody {
		err = fmt.Errorf("请求体超过 %d 字节", maxBody)
	}
	if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		err = json.Unmarshal(raw, out)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"encode response failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(raw)
}

// writeToolError answers the error of a tool call as the plugin host
// expects it, {"error": "…"}, redacted (redact.Text).
func writeToolError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusOK, map[string]any{"error": redact.Text(err.Error())})
}

// writeError answers err, redacted (redact.Text) as every error text of
// the API answers is.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": redact.Text(err.Error())})
}
