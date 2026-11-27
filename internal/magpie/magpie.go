// Package magpie implements host.Host against a running Magpie.
//
// Sources, by method:
//
//   - Accounts: GET /v1/magpie/accounts on the gateway (MAGPIE_ADDR).
//   - Credential: Magpie's configuration directory (logins.json), and the
//     proxies in providers.json and settings.json, for Codex and Claude
//     accounts. Tokens are only read, never refreshed.
//   - ConfirmIdentity: the agy-cli plugin's sign-ins and agy's own sign-in
//     files (agy.go), read only to confirm whose sign-in ignition will use.
//   - KeptQuotas: GET /v1/magpie/quotas?kept=1, which sends no request to a
//     vendor. It is the only source of Antigravity quota.
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
	"github.com/PosvdM/magpie-lamplighter/internal/redact"
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

	// mu guards refreshed and cardErrors, which live as long as the Host.
	mu sync.Mutex
	// refreshed is the last RefreshQuota attempt of each account pin.
	refreshed map[string]time.Time
	// cardErrors is the last error logged for the kept card of each
	// account pin (noteCardError).
	cardErrors map[string]string
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

var (
	_ host.Host            = (*Host)(nil)
	_ host.IdentityChecker = (*Host)(nil)
	_ host.KeptRefresher   = (*Host)(nil)
)

// Log writes one line: the level and the message, without emails,
// credentials or URL parameters (redact.Text). The fields are left out:
// the message of a record that has them carries them as JSON.
func (h *Host) Log(level, message string, fields map[string]any) {
	logger := h.Logger
	if logger == nil {
		logger = log.Default()
	}
	if level == "debug" && os.Getenv("LAMPLIGHTER_DEBUG") == "" {
		return
	}
	logger.Printf("[%s] %s", level, redact.Text(message))
}
