package magpie

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// warmupSettings are the fields of Magpie's settings.json that start a
// subscription's windows by themselves (Magpie's internal/settings, read by
// provider.KeepCodexWindowsWarm and KeepClaudeWindowsWarm):
//
//   - codexWarmup, claudeWarmup: "" or "off" for off, "week" for the weekly
//     window on its reset, "all" for the 5-hour one too.
//   - codexWarmAts, claudeWarmAts: times of day ("06:00") at which an
//     account whose 5-hour window is not running gets one. codexWarmAt and
//     claudeWarmAt are the single time an older Magpie wrote, used when the
//     list is absent.
//   - codexWarmAtOf: a ChatGPT account's own time of day by its user name in
//     lower case, "off" for none, in place of codexWarmAts.
//
// The warm-up runs whatever the "Warm-up and check-in" built-in plugin
// shows: that plugin is only the settings' place on the page.
type warmupSettings struct {
	CodexWarmup   *string           `json:"codexWarmup"`
	ClaudeWarmup  *string           `json:"claudeWarmup"`
	CodexWarmAt   *string           `json:"codexWarmAt"`
	ClaudeWarmAt  *string           `json:"claudeWarmAt"`
	CodexWarmAts  []string          `json:"codexWarmAts"`
	ClaudeWarmAts []string          `json:"claudeWarmAts"`
	CodexWarmAtOf map[string]string `json:"codexWarmAtOf"`
}

// MagpieWarmup returns the pins of the accounts whose 5-hour window
// Magpie's own warm-up starts: the warm-up on reset set to "all", or a time
// of day. Together with Lamplighter's ignition such an account is ignited
// twice. The weekly warm-up alone is left out: it sends one request when
// the week resets and does not schedule the 5-hour window.
//
// Accounts are matched by provider ("codex" or "claude") and, for a
// ChatGPT account's own time of day, by Email, which is Magpie's user name.
// A missing settings.json means no warm-up. A file that is not this format
// is an error wrapping host.ErrFormat.
func (h *Host) MagpieWarmup(accounts []host.Account) ([]string, error) {
	pins := []string{}
	if strings.TrimSpace(h.ConfigDir) == "" {
		return pins, nil
	}
	raw, err := os.ReadFile(filepath.Join(h.ConfigDir, "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return pins, fmt.Errorf("magpie warm-up: %w", err)
	}
	s, err := parseWarmup(raw)
	if err != nil {
		return pins, err
	}
	for _, account := range accounts {
		if s.fiveHour(account) {
			pins = append(pins, account.Pin)
		}
	}
	return pins, nil
}

func parseWarmup(raw []byte) (warmupSettings, error) {
	var s warmupSettings
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(raw)) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return warmupSettings{}, fmt.Errorf("magpie warm-up: %w: settings.json: %v", host.ErrFormat, err)
	}
	for _, mode := range []*string{s.CodexWarmup, s.ClaudeWarmup} {
		if mode == nil {
			continue
		}
		switch *mode {
		case "", "off", "week", "all":
		default:
			return warmupSettings{}, fmt.Errorf("magpie warm-up: %w: warm-up %q", host.ErrFormat, *mode)
		}
	}
	return s, nil
}

func warmValue(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// warmTimes are the times of day of one provider: the list, else the single
// time of an older Magpie.
func warmTimes(list []string, single *string) bool {
	if list != nil {
		for _, at := range list {
			if strings.TrimSpace(at) != "" {
				return true
			}
		}
		return false
	}
	return warmValue(single) != ""
}

// fiveHour reports whether Magpie's warm-up starts account's 5-hour window.
func (s warmupSettings) fiveHour(account host.Account) bool {
	switch strings.ToLower(strings.TrimSpace(account.Provider)) {
	case "codex":
		if warmValue(s.CodexWarmup) == "all" {
			return true
		}
		user := strings.ToLower(strings.TrimSpace(account.Email))
		for name, at := range s.CodexWarmAtOf {
			if user != "" && strings.ToLower(strings.TrimSpace(name)) == user {
				at = strings.ToLower(strings.TrimSpace(at))
				if at != "" {
					return at != "off"
				}
			}
		}
		return warmTimes(s.CodexWarmAts, s.CodexWarmAt)
	case "claude":
		return warmValue(s.ClaudeWarmup) == "all" || warmTimes(s.ClaudeWarmAts, s.ClaudeWarmAt)
	}
	return false
}
