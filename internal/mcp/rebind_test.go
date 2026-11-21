package mcp

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// startBlocked sends a page request to addr in the background and waits
// until web has it. The returned channel yields the status code, or 0 when
// the request failed.
func startBlocked(t *testing.T, addr string, web *blockingWeb) <-chan int {
	t.Helper()
	result := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/status", nil)
		req.Header.Set("Authorization", "Bearer admin")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			result <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		result <- resp.StatusCode
	}()
	awaitSignal(t, web.started, "page request did not start")
	return result
}

// Moving the listener answers the request that moved it, such as the
// PATCH /api/config that changed public_listen: Configure returns at once,
// the new address serves, and the old connection gets its response.
func TestRebindAnswersRequestInProgress(t *testing.T) {
	addr, moved := freeAddr(t), freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	web := &blockingWeb{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan bool, 1)}
	s.SetWeb(web)
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	result := startBlocked(t, addr, web)

	configured := make(chan struct{})
	go func() {
		s.Configure(Config{Listen: moved, WebEnabled: true, AdminKey: "admin", MCPEnabled: true, MCPKey: "mcp"})
		close(configured)
	}()
	awaitSignal(t, configured, "Configure waited for the request in progress")
	if p := s.Public(); !p.Running || p.Listen != moved {
		t.Fatalf("public %+v", p)
	}
	// The old address takes no new connections.
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("the old listener still accepts connections")
	}
	// The new one serves, here /mcp, while the old request is in progress.
	client := &http.Client{Timeout: 5 * time.Second}
	if code, _ := post(t, client, "http://"+moved+"/mcp", "POST", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "mcp", "", ""); code != 200 {
		t.Fatalf("new listener: %d", code)
	}
	close(web.release)
	if <-web.canceled {
		t.Fatal("the rebind canceled the page request")
	}
	if code := <-result; code != http.StatusOK {
		t.Fatalf("the request in progress got %d", code)
	}
}

// A request that does not finish within drainTimeout loses its connection.
func TestRebindDrainTimeout(t *testing.T) {
	old := drainTimeout
	drainTimeout = 200 * time.Millisecond
	t.Cleanup(func() { drainTimeout = old })
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	web := &blockingWeb{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan bool, 1)}
	s.SetWeb(web)
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	result := startBlocked(t, addr, web)
	s.Configure(Config{Listen: addr})
	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("got %d, want a closed connection", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the drain did not end")
	}
	select {
	case canceled := <-web.canceled:
		if !canceled {
			t.Fatal("the handler was not canceled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler is still running")
	}
}

// Close ends a drain at once instead of waiting for drainTimeout.
func TestCloseEndsDrain(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	web := &blockingWeb{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan bool, 1)}
	s.SetWeb(web)
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	result := startBlocked(t, addr, web)
	s.Configure(Config{Listen: freeAddr(t), WebEnabled: true, AdminKey: "admin"})
	closed := make(chan struct{})
	start := time.Now()
	go func() { s.Close(); close(closed) }()
	awaitSignal(t, closed, "Close waited for the drain")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	<-result
}

// Close cancels the page and API requests of the running listener and
// waits for their handlers: server.Server.Shutdown relies on it to know
// that no request of this listener is left.
func TestCloseCancelsWebRequests(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	web := &blockingWeb{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan bool, 1)}
	s.SetWeb(web)
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	result := startBlocked(t, addr, web)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	awaitSignal(t, closed, "Close waited for a page request")
	select {
	case canceled := <-web.canceled:
		if !canceled {
			t.Fatal("the handler was not canceled")
		}
	default:
		t.Fatal("Close returned with the handler running")
	}
	<-result
}

// A refused configuration serves nothing and reports why.
func TestRefusedConfig(t *testing.T) {
	addr := freeAddr(t)
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	s.SetWeb(webHandler())
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin", MCPEnabled: true, MCPKey: "mcp"})
	if p := s.Public(); !p.Running {
		t.Fatalf("public %+v", p)
	}
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "same", MCPEnabled: true, MCPKey: "same", Refused: "refused"})
	if p, st := s.Public(), s.State(); p.Running || p.Web || p.MCP || p.Error != "refused" || st.Running || st.Error != "refused" || !st.Enabled {
		t.Fatalf("refused: %+v %+v", p, st)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("a refused configuration kept the port: %v", err)
	}
	ln.Close()
	// With /mcp off, the reason is on public only.
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "same", MCPKey: "same", Refused: "refused"})
	if p, st := s.Public(), s.State(); p.Error != "refused" || st.Error != "" {
		t.Fatalf("refused without /mcp: %+v %+v", p, st)
	}
	s.Configure(Config{Listen: addr, WebEnabled: true, AdminKey: "admin"})
	if p := s.Public(); !p.Running || !p.Web || p.Error != "" {
		t.Fatalf("after the fix: %+v", p)
	}
}
