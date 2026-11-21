package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// blockingHost lists no accounts. Once block is set, Accounts and
// RefreshQuota hold the background loop until their context ends, which
// happens when the engine stops.
type blockingHost struct {
	block   atomic.Bool
	calls   atomic.Int32 // calls while blocking
	blocked chan struct{}
	once    sync.Once
}

func (h *blockingHost) wait(ctx context.Context) error {
	if !h.block.Load() {
		return nil
	}
	h.calls.Add(1)
	h.once.Do(func() { close(h.blocked) })
	<-ctx.Done()
	return ctx.Err()
}

func (h *blockingHost) Accounts(ctx context.Context) ([]host.Account, error) {
	return nil, h.wait(ctx)
}
func (h *blockingHost) Credential(context.Context, host.Account) (host.Credential, error) {
	return host.Credential{}, errors.New("no credential")
}
func (h *blockingHost) KeptQuotas(context.Context) ([]host.KeptQuota, error) { return nil, nil }
func (h *blockingHost) RefreshQuota(ctx context.Context, _ string) error     { return h.wait(ctx) }
func (h *blockingHost) Models(context.Context) ([]string, error)             { return nil, nil }
func (h *blockingHost) OpenModelStream(context.Context, host.ModelRequest) (host.ModelStream, error) {
	return host.ModelStream{}, errors.New("no stream")
}
func (h *blockingHost) ReadModelStream(string) (host.StreamChunk, error) {
	return host.StreamChunk{}, io.EOF
}
func (h *blockingHost) CloseModelStream(string) error      { return nil }
func (h *blockingHost) Log(string, string, map[string]any) {}

// newServerIn is newServer that also returns the path of config.json.
func newServerIn(t *testing.T) (*Server, *config.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.FileName)
	store, cfg, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.Options{Host: &blockingHost{blocked: make(chan struct{})}, Version: "test", DataDir: filepath.Dir(path)})
	s := New(store, cfg, nil, e, "test")
	t.Cleanup(s.Shutdown)
	return s, store, path
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// Shutdown does not wait for management requests that wait for a busy
// background loop: it cancels the engine before it waits for the requests
// of the public listener, and a queued request is not run. The engine
// starts after its startup delay of about 5 seconds.
func TestShutdownEndsWaitingManagementRequests(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, cfg, err := config.Open(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	h := &blockingHost{blocked: make(chan struct{})}
	e := engine.New(engine.Options{Host: h, Version: "test", DataDir: dir})
	s := New(store, cfg, nil, e, "test")
	t.Cleanup(s.Shutdown)
	public := freePort(t)
	if code, out := do(t, s.Handler(), http.MethodPatch, "/api/config", `{"public_listen":"`+public+`","web_enabled":true,"admin_key":"adm"}`); code != http.StatusOK {
		t.Fatalf("patch %d %v", code, out)
	}
	s.Start()
	deadline := time.Now().Add(15 * time.Second)
	for !e.Status().Running {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.block.Store(true)

	// The first request holds the loop; the second one waits for it.
	client := &http.Client{Timeout: 30 * time.Second}
	codes := make(chan int, 2)
	send := func(url, auth string) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			codes <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		codes <- resp.StatusCode
	}
	go send("http://"+public+"/api/refresh", "adm")
	select {
	case <-h.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not reach the host")
	}
	internal := httptest.NewServer(s.Handler())
	defer internal.Close()
	go send(internal.URL+"/api/refresh", "")
	time.Sleep(200 * time.Millisecond)

	shut := make(chan struct{})
	start := time.Now()
	go func() { s.Shutdown(); close(shut) }()
	select {
	case <-shut:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown waits for the management requests")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v", elapsed)
	}
	for range 2 {
		select {
		case code := <-codes:
			if code != http.StatusServiceUnavailable && code != 0 {
				t.Fatalf("a waiting request got %d", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a request did not end")
		}
	}
	if calls := h.calls.Load(); calls != 1 {
		t.Fatalf("the host was called %d times after blocking; the queued request ran", calls)
	}
}

// PATCH /api/config after Shutdown answers 503 and writes nothing: the
// instance lock is released after Shutdown, and the next process may
// already use the file.
func TestPatchAfterShutdownIsRefused(t *testing.T) {
	s, store, path := newServerIn(t)
	s.Shutdown()
	code, out := do(t, s.Handler(), http.MethodPatch, "/api/config", `{"config_token":"late"}`)
	if code != http.StatusServiceUnavailable || out["error"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if len(store.Raw()) != 0 {
		t.Fatalf("saved %v", store.Raw())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config.json written: %v", err)
	}
}

// Shutdown waits for a configuration write in progress, and a PATCH that
// waits behind it is refused without writing.
func TestShutdownWaitsForPatchInProgress(t *testing.T) {
	s, _, path := newServerIn(t)
	h := s.Handler()
	saved := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s.patched = func() {
		if calls.Add(1) == 1 {
			close(saved)
			<-release
		}
	}
	first := make(chan int, 1)
	go func() {
		code, _ := do(t, h, http.MethodPatch, "/api/config", `{"config_token":"first"}`)
		first <- code
	}()
	<-saved
	shut := make(chan struct{})
	go func() { s.Shutdown(); close(shut) }()
	second := make(chan int, 1)
	go func() {
		code, _ := do(t, h, http.MethodPatch, "/api/config", `{"config_token":"second"}`)
		second <- code
	}()
	select {
	case <-shut:
		t.Fatal("Shutdown returned during a configuration write")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-shut:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the write")
	}
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first %d", code)
	}
	if code := <-second; code != http.StatusServiceUnavailable {
		t.Fatalf("second %d", code)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil || file["config_token"] != "first" {
		t.Fatalf("file %s %v", raw, err)
	}
}
