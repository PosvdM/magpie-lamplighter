package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// mcpRefresh runs refresh_quotas for the account pin, with the engine loop
// replaced by one command.
func mcpRefresh(t *testing.T, e *Engine, pin string) map[string]any {
	t.Helper()
	s := e.Status()
	label := ""
	for i, l := range accountLabels(s.Accounts) {
		if s.Accounts[i].AuthIndex == pin {
			label = l
		}
	}
	if label == "" {
		t.Fatalf("no label for %s", pin)
	}
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	go func() {
		cmd := <-e.cmdCh
		e.runCommand(context.Background(), cmd)
	}()
	value, err := NewMCPBackend(e).refresh(context.Background(), map[string]string{"account": label})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := value.(map[string]any)
	return result
}

// A refresh Magpie cannot run tells why in the results of POST /api/refresh
// and refresh_quotas.
func TestManualRefreshReportsWhyMagpieCannotRefresh(t *testing.T) {
	for _, tc := range []struct{ code, reason string }{
		{"account_off", "账号在 Magpie 中已关闭"},
		{"account_not_found", "Magpie 中找不到这个账号"},
		{"gateway_unavailable", "Magpie 网关不可用"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			e, h, renew, _ := newRenewEngine(t)
			h.kept = nil
			renew.refresh = func(context.Context, string) error {
				return &host.Error{Method: "RefreshQuota", Code: tc.code, Message: "secret me@example.com"}
			}
			err := manualRefresh(e, "3")
			var failure *refreshFailure
			if !errors.As(err, &failure) || failure.reason != tc.reason || !strings.Contains(err.Error(), tc.reason) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("POST /api/refresh error %v", err)
			}
			result := mcpRefresh(t, e, "3")
			if result["ok"] != false || result["error"] != "Magpie 刷新额度失败："+tc.reason {
				t.Fatalf("refresh_quotas %#v", result)
			}
		})
	}
}

// Other errors keep their text for the page, and refresh_quotas does not
// pass it on.
func TestManualRefreshOtherErrors(t *testing.T) {
	e, h, renew, _ := newRenewEngine(t)
	h.kept = nil
	renew.refresh = func(context.Context, string) error { return errors.New("secret me@example.com") }
	if err := manualRefresh(e, "3"); err == nil || !strings.Contains(err.Error(), "Magpie 刷新额度失败") {
		t.Fatalf("POST /api/refresh error %v", err)
	}
	result := mcpRefresh(t, e, "3")
	if text, _ := result["error"].(string); result["ok"] != false || text == "" || strings.Contains(text, "secret") || strings.Contains(text, "example.com") {
		t.Fatalf("refresh_quotas %#v", result)
	}
}

// An account that is off or gone by the time of the refresh is reported,
// not taken for a refresh that worked.
func TestManualRefreshOfAnAccountGone(t *testing.T) {
	e, h, _, _ := newRenewEngine(t)
	for i := range h.auths {
		if h.auths[i].AuthIndex == "3" {
			h.auths[i].Disabled = true
		}
	}
	if err := manualRefresh(e, "3"); !errors.Is(err, errRefreshAccountGone) {
		t.Fatalf("error %v", err)
	}
	if err := manualRefresh(e, "missing"); !errors.Is(err, errRefreshAccountGone) {
		t.Fatalf("error %v", err)
	}
	if err := manualRefresh(e, ""); err != nil {
		t.Fatalf("refreshing all: %v", err)
	}
}
