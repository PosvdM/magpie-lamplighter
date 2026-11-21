package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
)

// gatewayRefreshHost is the engine's fake host, except that RefreshQuota
// is that of a magpie.Host on a fake Magpie gateway. The fake host's pins
// are bare numbers; Magpie's are provider/id, so pin "3" is "claude/3".
type gatewayRefreshHost struct {
	*expiredHost
	gateway *magpie.Host
}

func (h *gatewayRefreshHost) RefreshQuota(ctx context.Context, pin string) error {
	return h.gateway.RefreshQuota(ctx, "claude/"+pin)
}

// The codes magpie.Host.RefreshQuota gives when Magpie cannot refresh an
// account reach the results of POST /api/refresh and refresh_quotas as
// their reasons.
func TestManualRefreshReasonsFromTheMagpieGateway(t *testing.T) {
	const listed = `{"object":"list","data":[{"provider":"claude","id":"3","pin":"claude/3","kind":"account","user":"c@example.com","on":%s}]}`
	for _, tc := range []struct {
		name     string
		accounts string // the GET /v1/magpie/accounts answer; "" answers 502
		reason   string
	}{
		{"account_off", strings.Replace(listed, "%s", "false", 1), "账号在 Magpie 中已关闭"},
		{"account_not_found", `{"object":"list","data":[]}`, "Magpie 中找不到这个账号"},
		{"gateway_unavailable", "", "Magpie 网关不可用"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/magpie/accounts" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusNotFound)
					return
				}
				if tc.accounts == "" {
					http.Error(w, `{"error":{"message":"down"}}`, http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.accounts))
			}))
			t.Cleanup(gateway.Close)
			e, h, renew, _ := newRenewEngine(t)
			h.kept = nil
			e.host = &gatewayRefreshHost{
				expiredHost: renew.expiredHost,
				gateway:     &magpie.Host{Addr: strings.TrimPrefix(gateway.URL, "http://"), ConfigDir: t.TempDir()},
			}
			err := manualRefresh(e, "3")
			if err == nil || !strings.Contains(err.Error(), tc.reason) || strings.Contains(err.Error(), "example.com") {
				t.Fatalf("POST /api/refresh error %v, want the reason %q", err, tc.reason)
			}
			result := mcpRefresh(t, e, "3")
			if result["ok"] != false || result["error"] != "Magpie 刷新额度失败："+tc.reason {
				t.Fatalf("refresh_quotas %#v", result)
			}
		})
	}
}

// loggedGatewayHost is a gatewayRefreshHost that records the log.
type loggedGatewayHost struct {
	*gatewayRefreshHost
	mu    sync.Mutex
	lines []string
}

func (h *loggedGatewayHost) Log(level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, fmt.Sprint(level, " ", message, " ", fields))
}

// A manual refresh that runs out of time while Magpie's gateway has not
// answered the refresh shows no part of its URL, whose query names the
// account by email: not in the result of POST /api/refresh, the
// account's error or the log.
func TestManualRefreshTimeoutShowsNoEmail(t *testing.T) {
	shortManualBudget(t, 800*time.Millisecond, 200*time.Millisecond)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/magpie/accounts":
			w.Write([]byte(`{"object":"list","data":[{"provider":"claude","id":"3","pin":"claude/3","kind":"account","user":"carol.secret@example.com","on":true}]}`))
		case "GET /v1/magpie/quotas":
			w.Write([]byte(`{"object":"list","data":[]}`))
		default:
			io.ReadAll(r.Body)
			<-r.Context().Done()
		}
	}))
	t.Cleanup(gateway.Close)
	e, h, renew, c := newRenewEngine(t)
	h.kept = []host.KeptQuota{keptClaude("3", c.t, 50)}
	logged := &loggedGatewayHost{gatewayRefreshHost: &gatewayRefreshHost{
		expiredHost: renew.expiredHost,
		gateway:     &magpie.Host{Addr: strings.TrimPrefix(gateway.URL, "http://"), ConfigDir: t.TempDir()},
	}}
	e.host = logged
	err := manualRefresh(e, "3")
	if err == nil || !strings.Contains(err.Error(), "超过手动刷新的时限") || !strings.Contains(err.Error(), "POST /v1/magpie/quotas/refresh") {
		t.Fatalf("err = %v", err)
	}
	shown := []string{err.Error()}
	for _, a := range e.Status().Accounts {
		shown = append(shown, a.Error)
	}
	logged.mu.Lock()
	shown = append(shown, logged.lines...)
	logged.mu.Unlock()
	for _, s := range shown {
		for _, part := range []string{"carol", "example.com", "user=", "http://"} {
			if strings.Contains(s, part) {
				t.Fatalf("%q shows %q", s, part)
			}
		}
	}
}
