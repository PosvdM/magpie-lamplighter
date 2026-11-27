package magpie

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// The gateway client follows no redirect: a gateway that answers a 302 or
// 307 pointing at another address fails the call with the 3xx status, and
// the other address is never dialed (WatchGatewayDials sees every dial).
func TestGatewayClientFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(other.Close)
	const pin = "agy-cli/ffeeddccbbaa9988"
	for _, tc := range []struct {
		route string
		code  int
		call  func(h *Host) (status int, err error)
	}{
		{"GET /v1/magpie/accounts", http.StatusFound, func(h *Host) (int, error) {
			_, err := h.Accounts(context.Background())
			return 0, err
		}},
		{"GET /v1/magpie/quotas", http.StatusFound, func(h *Host) (int, error) {
			_, err := h.KeptQuotas(context.Background())
			return 0, err
		}},
		{"POST /v1/magpie/quotas/refresh", http.StatusTemporaryRedirect, func(h *Host) (int, error) {
			_, found, err := h.RefreshKept(context.Background(), pin)
			if found {
				t.Error("found a card")
			}
			return 0, err
		}},
		{"POST /v1/chat/completions", http.StatusTemporaryRedirect, func(h *Host) (int, error) {
			opened, err := h.OpenModelStream(context.Background(), host.ModelRequest{Account: pin, Protocol: "openai", Model: "m", Body: []byte(`{"stream":true}`)})
			return opened.StatusCode, err
		}},
	} {
		t.Run(tc.route, func(t *testing.T) {
			elsewhere.Store(0)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch route := r.Method + " " + r.URL.Path; {
				case route == tc.route:
					http.Redirect(w, r, other.URL+r.URL.RequestURI(), tc.code)
				case route == "GET /v1/magpie/accounts":
					w.Write([]byte(accountsWithPlansJSON))
				case route == "GET /v1/magpie/quotas":
					w.Write([]byte(`{"data":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(gateway.Close)
			addr := strings.TrimPrefix(gateway.URL, "http://")
			var mu sync.Mutex
			var dialed []string
			restore := WatchGatewayDials(func(ctx context.Context, network, a string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, a)
				mu.Unlock()
				return (&net.Dialer{}).DialContext(ctx, network, a)
			})
			t.Cleanup(restore)
			status, err := tc.call(&Host{Addr: addr, ConfigDir: t.TempDir()})
			var hostErr *host.Error
			if !errors.As(err, &hostErr) || hostErr.Status != tc.code {
				t.Fatalf("err = %v", err)
			}
			if status != 0 && status != tc.code {
				t.Fatalf("status %d", status)
			}
			if n := elsewhere.Load(); n != 0 {
				t.Fatalf("reached the other address %d times", n)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(dialed) == 0 {
				t.Fatal("no dial seen")
			}
			for _, a := range dialed {
				if a != addr {
					t.Fatalf("dialed %s", a)
				}
			}
		})
	}
}
