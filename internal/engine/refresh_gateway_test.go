package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
