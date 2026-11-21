// Package magpie implements host.Host against a running Magpie.
//
// Sources, by method:
//
//   - Accounts: GET /v1/magpie/accounts on the gateway (MAGPIE_ADDR).
//   - Credential: Magpie's configuration directory (logins.json and the
//     agy-cli bridge's account files), and the proxies in providers.json and
//     settings.json. Tokens that rotate on refresh (Claude, Codex) are only
//     read, never refreshed; a Google token is refreshed in memory.
//   - KeptQuotas: GET /v1/magpie/quotas?kept=1, which sends no request to a
//     vendor.
//   - RefreshQuota: POST /v1/magpie/quotas/refresh.
//   - Models: GET /v1/models on the gateway.
//   - OpenModelStream and the rest: POST to the gateway with
//     X-Magpie-Account set to the account pin.
//
// A file or response whose format is not the one described here is an
// error wrapping host.ErrFormat; nothing is guessed.
package magpie

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// Host talks to one Magpie.
type Host struct {
	// Addr is the gateway address, such as "127.0.0.1:3425".
	Addr string
	// ConfigDir is Magpie's configuration directory, such as
	// $XDG_CONFIG_HOME/magpie.
	ConfigDir string
	// Logger receives the log entries; nil writes to standard error.
	Logger *log.Logger

	// mu guards google and refreshed, which live as long as the Host.
	mu sync.Mutex
	// google holds the Google access tokens renewed in memory, by the
	// fingerprint of their refresh token.
	google map[string]googleToken
	// refreshed is the last RefreshQuota attempt of each account pin.
	refreshed map[string]time.Time
	// streams holds the open model streams.
	streams streams
}

// FromEnv returns a Host for the Magpie that started this process:
// MAGPIE_ADDR, and MAGPIE_CONFIG_DIR, else magpie under XDG_CONFIG_HOME,
// else ~/.config/magpie.
func FromEnv() *Host {
	dir := os.Getenv("MAGPIE_CONFIG_DIR")
	if dir == "" {
		if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
			dir = base + "/magpie"
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = home + "/.config/magpie"
		}
	}
	return &Host{Addr: strings.TrimSpace(os.Getenv("MAGPIE_ADDR")), ConfigDir: dir}
}

var _ host.Host = (*Host)(nil)

// Log writes one line: the level, the message and the fields as JSON when
// there are any.
func (h *Host) Log(level, message string, fields map[string]any) {
	logger := h.Logger
	if logger == nil {
		logger = log.Default()
	}
	if level == "debug" && os.Getenv("LAMPLIGHTER_DEBUG") == "" {
		return
	}
	logger.Printf("[%s] %s", level, message)
}
