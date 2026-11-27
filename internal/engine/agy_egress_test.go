package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// agyGateway is a fake Magpie gateway serving one agy-cli account: its
// account list, its quota card, refreshes of the card, the model list and
// ignition requests. A refresh after an ignition reads the window the
// ignition started.
type agyGateway struct {
	*httptest.Server
	mu        sync.Mutex
	readAt    time.Time
	ignitedAt time.Time
	requests  []string
}

const agyEgressUser = "c@example.com"

// agyEgressPin is the pin Magpie gives the agy-cli account agyEgressUser.
func agyEgressPin() string {
	sum := sha256.Sum256([]byte("magpie login\nagy-cli\n" + agyEgressUser))
	return "agy-cli/" + hex.EncodeToString(sum[:8])
}

func newAgyGateway(t *testing.T) *agyGateway {
	g := &agyGateway{readAt: time.Now().Add(-20 * time.Minute)}
	pin := agyEgressPin()
	id := strings.TrimPrefix(pin, "agy-cli/")
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.requests = append(g.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		card := func() string {
			used, reset := 0.0, g.readAt.Add(-time.Minute)
			if !g.ignitedAt.IsZero() {
				used, reset = 0.5, g.ignitedAt.Add(5*time.Hour)
			}
			b, _ := json.Marshal(map[string]any{"data": []any{map[string]any{
				"provider": "agy-cli", "kind": "subscription", "user": agyEgressUser, "readAt": g.readAt.UTC().Format(time.RFC3339Nano),
				"windows": []any{map[string]any{"name": "Gemini · 5 hours", "used": used, "resetsAt": reset.UTC().Format(time.RFC3339), "pool": "Gemini"}},
			}}})
			return string(b)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/magpie/accounts":
			fmt.Fprintf(w, `{"object":"list","data":[{"provider":"agy-cli","id":%q,"pin":%q,"kind":"account","label":%q,"user":%q,"plan":"Google AI Pro","on":true}]}`, id, pin, agyEgressUser, agyEgressUser)
		case "GET /v1/magpie/quotas":
			io.WriteString(w, card())
		case "POST /v1/magpie/quotas/refresh":
			g.readAt = time.Now()
			io.WriteString(w, card())
		case "GET /v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"agy-cli/gemini-3.5-flash","owned_by":"agy-cli"}]}`)
		case "POST /v1/chat/completions":
			io.Copy(io.Discard, r.Body)
			g.ignitedAt = time.Now()
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			http.Error(w, `{"error":{"message":"no route"}}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *agyGateway) sent(route string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if r == route {
			n++
		}
	}
	return n
}

// agyEgressFiles writes the agy-cli plugin's sign-in of agyEgressUser in
// the shape of agy-cli 0.1.151, and returns Magpie's configuration
// directory.
func agyEgressFiles(t *testing.T) string {
	config, state := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", t.TempDir())
	const home = "0123456789abcdef"
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(config, "logins.json"), `[{"agent":"plugin:agy-cli","user":"`+agyEgressUser+`","home":"agy-cli#k3x9a2"}]`)
	write(filepath.Join(config, "plugin-auth.json"), `{"agy-cli#k3x9a2":{"type":"api","key":"agy:`+home+`","metadata":{"email":"`+agyEgressUser+`","agyHome":"`+home+`"}}}`)
	token, _ := json.Marshal(map[string]any{
		"token":          map[string]any{"access_token": "ya29.x", "token_type": "Bearer", "refresh_token": "1//x", "expiry": time.Now().Add(-time.Hour).Format(time.RFC3339), "expires_in": 3599},
		"auth_method":    "consumer",
		"id_token_email": agyEgressUser,
	})
	write(filepath.Join(state, "magpie-agy-bridge", "accounts", home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), string(token))
	return config
}

// egressGuard dials for a test: it dials only allowed and records every
// other address it is asked to reach, and counts the dials it let through.
// One serves as http.DefaultTransport, which package egress, sending the
// quota requests of Codex and Claude, clones; another dials for the
// gateway's own client (magpie.WatchGatewayDials).
type egressGuard struct {
	allowed string
	mu      sync.Mutex
	refused []string
	dialed  int
}

func (g *egressGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if addr == g.allowed {
		g.dialed++
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	g.refused = append(g.refused, addr)
	return nil, errors.New("egress guard: not Magpie")
}

func (g *egressGuard) dials() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dialed
}

func (g *egressGuard) addrs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.refused...)
}

// useEgressGuard makes every request through http.DefaultTransport, and
// through package egress, that does not go to allowed fail and be
// recorded, also through a proxy.
func useEgressGuard(t *testing.T, allowed string) *egressGuard {
	g := &egressGuard{allowed: allowed}
	was := http.DefaultTransport
	http.DefaultTransport = &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: g.dial}
	t.Cleanup(func() { http.DefaultTransport = was })
	return g
}

// useGatewayGuard makes every connection of the gateway client that does
// not go to allowed fail and be recorded.
func useGatewayGuard(t *testing.T, allowed string) *egressGuard {
	g := &egressGuard{allowed: allowed}
	t.Cleanup(magpie.WatchGatewayDials(g.dial))
	return g
}

// Lamplighter sends an agy-cli account's requests to Magpie alone: the
// confirmation of its sign-in reads local files, and its scheduled query,
// manual refresh, ignition and the confirmation of the ignition reach no
// other address, through the gateway client, package egress or Go's
// default transport; every connection of the gateway client goes to
// MAGPIE_ADDR. The test runs Magpie's real Host and the engine's real
// egress; it waits magpieConfirmAfter for the confirmation.
func TestAgyAccountReachesOnlyMagpie(t *testing.T) {
	for _, v := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(v, "")
	}
	gateway := newAgyGateway(t)
	addr := strings.TrimPrefix(gateway.URL, "http://")
	guard := useEgressGuard(t, addr)
	gatewayGuard := useGatewayGuard(t, addr)
	configDir := agyEgressFiles(t)
	newHost := func() *magpie.Host { return &magpie.Host{Addr: addr, ConfigDir: configDir} }

	ctx := context.Background()
	e := New(Options{Host: newHost(), Version: "test", DataDir: t.TempDir()})
	cfg := config.Default()
	cfg.Timezone = "Asia/Shanghai"
	cfg.Ignition.Enabled = true
	p := cfg.Provider("antigravity")
	p.Monitor, p.Ignite = true, true
	cfg.Providers["antigravity"] = p
	e.Configure(cfg, nil)
	if err := e.openStores(cfg); err != nil {
		t.Fatal(err)
	}
	e.applyRuntimeConfig(cfg)
	pin := agyEgressPin()

	// The sign-in is confirmed from the local files.
	if fp, err := e.host.(*magpie.Host).ConfirmIdentity(ctx, hostAccount(&Cred{AuthIndex: pin, HostProvider: "agy-cli"})); err != nil || fp == "" {
		t.Fatalf("ConfirmIdentity = %q, %v", fp, err)
	}
	// A scheduled round reads the sign-in and Magpie's stale reading, and
	// the worker has Magpie refresh it.
	e.poll(ctx, e.config(), time.Now(), "", false)
	workAgy(e)
	if gateway.sent("POST /v1/magpie/quotas/refresh") != 1 || e.creds[pin] == nil {
		t.Fatalf("requests %v", gateway.requests)
	}
	// A manual refresh, through a host without the last refresh's gap.
	e.host = newHost()
	if err := manualRefresh(e, pin); err != nil {
		t.Fatalf("manual refresh: %v", err)
	}
	// The 5-hour window has ended: ignition confirms the sign-in, sends
	// the request through the gateway, and confirms the window with a
	// refresh.
	e.performDue(ctx, e.config())
	key := agyTarget(t, e).group.Key
	if gateway.sent("POST /v1/chat/completions") != 1 || !e.confirmPending(key) {
		t.Fatalf("requests %v, events %+v", gateway.requests, e.Status().Events)
	}
	time.Sleep(magpieConfirmAfter + 300*time.Millisecond)
	workAgy(e)
	if e.confirmPending(key) || len(eventsOf(e, "ignite")) != 1 {
		t.Fatalf("not confirmed: requests %v, events %+v", gateway.requests, e.Status().Events)
	}
	if refused := guard.addrs(); len(refused) != 0 {
		t.Fatalf("requests to %v", refused)
	}
	if refused := gatewayGuard.addrs(); len(refused) != 0 || gatewayGuard.dials() == 0 {
		t.Fatalf("gateway client dialed %d times to Magpie, and to %v", gatewayGuard.dials(), refused)
	}

	// The guard sees what package egress would send.
	if _, err := e.egress.Do(ctx, "", quota.Request{Method: http.MethodGet, URL: "https://example.invalid/"}); err == nil || len(guard.addrs()) != 1 {
		t.Fatalf("the guard let egress through: %v, %v", err, guard.addrs())
	}
}
