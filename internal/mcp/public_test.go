package mcp

import (
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

func get(t *testing.T, url, auth string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

func webHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>page") })
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) })
	return mux
}

func TestPublicWebNeedsAdminKey(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	s.SetWeb(webHandler())
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin", MCPKey: "unused"})
	if p := s.Public(); !p.Running || !p.Web || p.MCP || p.Error != "" || p.Listen != addr {
		t.Fatalf("public %+v", p)
	}
	if st := s.State(); st.Enabled || st.Error != "" || st.Path != "/mcp" {
		t.Fatalf("mcp %+v", st)
	}
	base := "http://" + addr
	// The page holds no data and loads without the key.
	if code, _, body := get(t, base+"/", ""); code != 200 || !strings.Contains(body, "page") {
		t.Fatalf("page %d %s", code, body)
	}
	for _, auth := range []string{"", "Bearer wrong", "admin", "Bearer admin2", "Basic admin"} {
		code, header, _ := get(t, base+"/api/status", auth)
		if code != 401 || header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("auth %q: %d", auth, code)
		}
	}
	// Unknown paths ask for the key too, so they do not reveal the routes.
	if code, _, _ := get(t, base+"/api/missing", ""); code != 401 {
		t.Fatalf("unknown route without key: %d", code)
	}
	if code, _, body := get(t, base+"/api/status", "Bearer admin"); code != 200 || body != `{"ok":true}` {
		t.Fatalf("with key %d %s", code, body)
	}
	if code, _, _ := get(t, base+"/api/missing", "Bearer admin"); code != 404 {
		t.Fatalf("unknown route with key: %d", code)
	}
	// /mcp is off: the admin key does not open it.
	client := &http.Client{Timeout: 5 * time.Second}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if code, _ := post(t, client, base+"/mcp", "POST", ping, "admin", "", ""); code != 404 {
		t.Fatalf("mcp while off: %d", code)
	}
}

func TestPublicKeysAreSeparate(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	s.SetWeb(webHandler())
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin", MCPEnabled: true, MCPKey: "mcp"})
	if p := s.Public(); !p.Running || !p.Web || !p.MCP {
		t.Fatalf("public %+v", p)
	}
	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if code, _ := post(t, client, base+"/mcp", "POST", ping, "admin", "", ""); code != 401 {
		t.Fatalf("admin key on /mcp: %d", code)
	}
	if code, _ := post(t, client, base+"/mcp", "POST", ping, "mcp", "", ""); code != 200 {
		t.Fatalf("mcp key on /mcp: %d", code)
	}
	if code, _, _ := get(t, base+"/api/status", "Bearer mcp"); code != 401 {
		t.Fatalf("mcp key on the API: %d", code)
	}
}

func TestPublicListenerToggles(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	s.SetWeb(webHandler())
	listener := func() *http.Server { s.mu.Lock(); defer s.mu.Unlock(); return s.http }

	// Both off, or on without keys: nothing to serve, so no listener.
	for _, c := range []Config{
		{Listen: addr},
		{Listen: addr, WebEnabled: true},
		{Listen: addr, AdminKey: "admin", MCPKey: "mcp"},
	} {
		s.Configure(c)
		if p := s.Public(); p.Running || p.Web || p.MCP || p.Error != "" {
			t.Fatalf("%+v: public %+v", c, p)
		}
	}
	s.Configure(Config{Listen: addr, MCPEnabled: true})
	if st, p := s.State(), s.Public(); st.Error != errMCPKeyEmpty || p.Running || p.Error != "" {
		t.Fatalf("empty MCP key: %+v %+v", st, p)
	}

	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	first := listener()
	if first == nil || !s.Public().Running {
		t.Fatalf("web listener %+v", s.Public())
	}
	base := "http://" + addr
	steps := []struct {
		config  Config
		web     bool
		mcp     bool
		mcpErr  string
		apiCode int
	}{
		{Config{Listen: addr, WebEnabled: true, AdminKey: "admin", MCPEnabled: true, MCPKey: "mcp"}, true, true, "", 200},
		{Config{Listen: addr, WebEnabled: true, AdminKey: "admin", MCPEnabled: true}, true, false, errMCPKeyEmpty, 200},
		{Config{Listen: addr, WebEnabled: true, AdminKey: "rotated", MCPEnabled: true, MCPKey: "mcp"}, true, true, "", 401},
		{Config{Listen: addr, AdminKey: "admin", MCPEnabled: true, MCPKey: "mcp"}, false, true, "", 404},
		{Config{Listen: addr, WebEnabled: true, MCPEnabled: true, MCPKey: "mcp"}, false, true, "", 404},
	}
	for i, step := range steps {
		s.Configure(step.config)
		if listener() != first {
			t.Fatalf("step %d rebound the listener", i)
		}
		p, st := s.Public(), s.State()
		if !p.Running || p.Web != step.web || p.MCP != step.mcp || st.Error != step.mcpErr || st.Enabled != step.config.MCPEnabled {
			t.Fatalf("step %d: %+v %+v", i, p, st)
		}
		if code, _, _ := get(t, base+"/api/status", "Bearer admin"); code != step.apiCode {
			t.Fatalf("step %d: API with the old key: %d", i, code)
		}
	}

	// A new address rebinds.
	moved := freeAddr(t)
	s.Configure(Config{Listen: moved, MCPEnabled: true, MCPKey: "mcp"})
	if listener() == first || s.Public().Listen != moved || !s.Public().Running {
		t.Fatalf("move %+v", s.Public())
	}
	s.Configure(Config{Listen: moved})
	if p := s.Public(); p.Running || p.Web || p.MCP {
		t.Fatalf("off %+v", p)
	}
	ln, err := net.Listen("tcp", moved)
	if err != nil {
		t.Fatalf("stopped listener kept the port: %v", err)
	}
	ln.Close()
}

func TestPublicBindErrorWithoutMCP(t *testing.T) {
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer reserve.Close()
	addr := reserve.Addr().String()
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	s.SetWeb(webHandler())
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	if p := s.Public(); p.Running || p.Web || p.Error == "" {
		t.Fatalf("public %+v", p)
	}
	// The bind error belongs to the listener; /mcp is off and shows none.
	if st := s.State(); st.Error != "" {
		t.Fatalf("mcp %+v", st)
	}
}

// blockingWeb blocks a page request until its context ends or release is
// closed.
type blockingWeb struct {
	started  chan struct{}
	release  chan struct{}
	canceled chan bool
}

func (b *blockingWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	close(b.started)
	select {
	case <-r.Context().Done():
		b.canceled <- true
	case <-b.release:
		b.canceled <- false
	}
}

func TestMCPKeyChangeKeepsWebRequests(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	web := &blockingWeb{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan bool, 1)}
	s.SetWeb(web)
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin", MCPEnabled: true, MCPKey: "mcp"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/status", nil)
		req.Header.Set("Authorization", "Bearer admin")
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	awaitSignal(t, web.started, "page request did not start")
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	close(web.release)
	if <-web.canceled {
		t.Fatal("turning /mcp off canceled a page request")
	}
	<-done
}
