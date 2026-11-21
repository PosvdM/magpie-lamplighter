package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/engine"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
	"github.com/PosvdM/magpie-lamplighter/internal/server"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

func TestCheckListen(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8317", "127.0.0.2:1", "[::1]:8317", "localhost:8317", "LOCALHOST:0", "127.0.0.1:0"} {
		if err := checkListen(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8317", ":8317", "[::]:8317", "192.168.1.2:8317", "example.com:8317", "localhost.example.com:8317", "127.0.0.1", ""} {
		if err := checkListen(addr); err == nil {
			t.Fatalf("%s: accepted", addr)
		}
	}
}

// run refuses a non-loopback -listen before it touches the data directory.
func TestRunRefusesNonLoopbackListen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := run(context.Background(), "0.0.0.0:0", dir, &magpie.Host{}); err == nil {
		t.Fatal("run accepted 0.0.0.0")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("data directory created: %v", err)
	}
}

// The write timeout leaves room for the longest requests: a tool call of
// up to 50 seconds and a page action that waits up to 3 minutes for the
// background loop.
func TestInternalServerTimeouts(t *testing.T) {
	s := newInternalServer(http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.IdleTimeout <= 0 || s.WriteTimeout <= 3*time.Minute {
		t.Fatalf("timeouts %v %v %v %v", s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout, s.WriteTimeout)
	}
}

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

func listening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		conn.Close()
	}
	return err == nil
}

func writeConfig(t *testing.T, dir string, cfg map[string]any) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A second process waits for the instance lock without listening on any
// port, then reads the configuration that the first one left.
func TestRunWaitsForTheInstanceLock(t *testing.T) {
	old := lockRetry
	lockRetry = 20 * time.Millisecond
	t.Cleanup(func() { lockRetry = old })
	dir := t.TempDir()
	internal, public := freeAddr(t), freeAddr(t)
	writeConfig(t, dir, map[string]any{"config_token": "before", "public_listen": public, "web_enabled": true, "admin_key": "k"})
	held, ok, err := store.TryLock(filepath.Join(dir, lockFile))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, internal, dir, &magpie.Host{}) }()
	time.Sleep(300 * time.Millisecond)
	if listening(internal) || listening(public) {
		t.Fatal("listening while another process holds the lock")
	}
	// The first process saves on its way out.
	writeConfig(t, dir, map[string]any{"config_token": "after", "public_listen": public, "web_enabled": true, "admin_key": "k"})
	held.Release()

	deadline := time.Now().Add(5 * time.Second)
	for !listening(internal) {
		if time.Now().After(deadline) {
			t.Fatal("not listening after the lock was released")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get("http://" + internal + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var saved map[string]any
	if err := json.Unmarshal(body, &saved); err != nil || saved["config_token"] != "after" {
		t.Fatalf("config %s %v", body, err)
	}
	if !listening(public) {
		t.Fatal("the public listener is not up")
	}
	if _, ok, _ := store.TryLock(filepath.Join(dir, lockFile)); ok {
		t.Fatal("run does not hold the lock")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
	if listening(internal) || listening(public) {
		t.Fatal("still listening after run returned")
	}
	again, ok, err := store.TryLock(filepath.Join(dir, lockFile))
	if err != nil || !ok {
		t.Fatalf("lock not released: %v %v", ok, err)
	}
	again.Release()
}

// A process that is told to stop while it waits exits without listening.
func TestRunStopsWhileWaiting(t *testing.T) {
	old := lockRetry
	lockRetry = 20 * time.Millisecond
	t.Cleanup(func() { lockRetry = old })
	dir := t.TempDir()
	held, ok, err := store.TryLock(filepath.Join(dir, lockFile))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer held.Release()
	internal := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, internal, dir, &magpie.Host{}) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run kept waiting for %s", lockFile)
	}
	if listening(internal) {
		t.Fatal("listened")
	}
}

// A process told to stop before it gets the lock does not wait lockRetry,
// and one told to stop as it gets the lock releases it at once. Neither
// reads config.json, which here does not parse, nor listens.
func TestRunStopsBeforeAndAfterTakingTheLock(t *testing.T) {
	old := lockRetry
	lockRetry = time.Hour
	t.Cleanup(func() { lockRetry = old })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	internal := freeAddr(t)

	// Free lock: taken, then released.
	if err := run(ctx, internal, dir, &magpie.Host{}); err != nil {
		t.Fatalf("free lock: %v", err)
	}
	if listening(internal) {
		t.Fatal("listened")
	}
	lock, ok, err := store.TryLock(filepath.Join(dir, lockFile))
	if err != nil || !ok {
		t.Fatalf("lock kept: %v %v", ok, err)
	}

	// Held lock: no wait of lockRetry.
	done := make(chan error, 1)
	go func() { done <- run(ctx, internal, dir, &magpie.Host{}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("held lock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run waited for the lock after ctx ended")
	}
	lock.Release()
}

// shutdown stops the server before the internal listener: a PATCH that
// arrives late is refused without writing, and a request that does not
// end loses its connection after shutdownTimeout.
func TestShutdownRefusesLatePatch(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })
	dir := t.TempDir()
	cfgStore, cfg, err := config.Open(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.Options{Host: &magpie.Host{}, Version: "test", DataDir: dir})
	srv := server.New(cfgStore, cfg, nil, e, "test")

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	mux := http.NewServeMux()
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	mux.Handle("/", srv.Handler())
	httpServer := newInternalServer(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go httpServer.Serve(ln)
	base := "http://" + ln.Addr().String()
	hung := make(chan error, 1)
	go func() {
		resp, err := http.Get(base + "/hang")
		if err == nil {
			resp.Body.Close()
		}
		hung <- err
	}()
	<-started

	start := time.Now()
	shutdown(srv, httpServer)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}
	select {
	case err := <-hung:
		if err == nil {
			t.Fatal("the hanging request got a response")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hanging request kept its connection")
	}
	// The PATCH a handler still running would send.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config_token":"late"}`))
	req.Host = "127.0.0.1:8317"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("late patch: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); !os.IsNotExist(err) {
		t.Fatalf("config.json written: %v", err)
	}
}
