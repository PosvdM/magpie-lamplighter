// Package server serves the page, the HTTP API and the tool calls of the
// Magpie plugin host, and applies configuration changes.
//
// The internal listener runs on loopback and takes no key: Magpie reaches
// it for the plugin page (magpie.page.service) and for the tools of the
// plugin host. It refuses a Host that is not loopback and opens its API
// to the sandboxed plugin page by a page token; see guardInternal. The
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
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/mcp"
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
}

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
func (s *Server) apply(cfg config.Config, parseErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	revision := s.engine.Configure(cfg, parseErr)
	if parseErr == nil {
		s.mcp.Configure(mcp.Config{
			Listen:     cfg.PublicListen,
			MCPEnabled: cfg.MCPEnabled,
			MCPKey:     cfg.MCPKey,
			WebEnabled: cfg.WebEnabled,
			AdminKey:   cfg.AdminKey,
		})
		s.engine.MCPConfigured(revision)
	}
}

// Start starts the engine.
func (s *Server) Start() { s.engine.Start() }

// Shutdown closes the public listener and stops the engine. It does not
// hold mu while the listener waits for its requests: a page or API request
// on it may be waiting for mu.
func (s *Server) Shutdown() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	s.mcp.Close()
	s.engine.Stop(stopTimeout)
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
		resp, err := s.engine.HistoryRange(r.URL.Query().Get("range"), r.URL.Query().Get("window"))
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
		s.action(w, s.engine.Refresh(strings.TrimSpace(body.AuthIndex)))
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
		s.action(w, s.engine.Ignite(strings.TrimSpace(body.Target)))
	})
	mux.HandleFunc("POST /api/test-notify", func(w http.ResponseWriter, r *http.Request) {
		results, err := s.engine.TestNotify()
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
		s.action(w, s.engine.SetLanguage(body.Language))
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
		cfg, err := s.store.Patch(patch)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.apply(cfg, nil)
		writeJSON(w, http.StatusOK, s.store.Raw())
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
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": result})
	})
	return mux
}

func (s *Server) action(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrNotRunning) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "status": s.status()})
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

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
