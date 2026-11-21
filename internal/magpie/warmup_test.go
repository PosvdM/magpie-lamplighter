package magpie

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// The engine finds MagpieWarmup through this interface (engine.warmupReader).
var _ interface {
	MagpieWarmup([]host.Account) ([]string, error)
} = (*Host)(nil)

var warmAccounts = []host.Account{
	{Pin: "codex/a", Provider: "codex", Email: "A@example.com"},
	{Pin: "codex/b", Provider: "codex", Email: "b@example.com"},
	{Pin: "claude/c", Provider: "claude", Email: "c@example.com"},
	{Pin: "antigravity/d", Provider: "antigravity", Email: "d@example.com"},
}

func warmupOf(t *testing.T, settings string) ([]string, error) {
	t.Helper()
	dir := t.TempDir()
	if settings != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return (&Host{ConfigDir: dir}).MagpieWarmup(warmAccounts)
}

func TestMagpieWarmup(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		want           []string
	}{
		{"no file", "", []string{}},
		{"empty file", "  ", []string{}},
		{"off", `{"codexWarmup":"","claudeWarmup":"off","theme":"dark"}`, []string{}},
		{"weekly only", `{"codexWarmup":"week","claudeWarmup":"week"}`, []string{}},
		{"codex all", `{"codexWarmup":"all"}`, []string{"codex/a", "codex/b"}},
		{"claude all", "\xef\xbb\xbf" + `{"claudeWarmup":"all"}`, []string{"claude/c"}},
		{"times of day", `{"codexWarmAts":["06:00"],"claudeWarmAts":["09:00","15:05"]}`, []string{"codex/a", "codex/b", "claude/c"}},
		{"older single time", `{"claudeWarmAt":"06:00"}`, []string{"claude/c"}},
		{"empty list beats single time", `{"codexWarmAt":"06:00","codexWarmAts":[]}`, []string{}},
		{"own time off", `{"codexWarmAts":["06:00"],"codexWarmAtOf":{"a@example.com":"off"}}`, []string{"codex/b"}},
		{"own time alone", `{"codexWarmAtOf":{"b@example.com":"07:30"}}`, []string{"codex/b"}},
		{"own off but all", `{"codexWarmup":"all","codexWarmAtOf":{"a@example.com":"off"}}`, []string{"codex/a", "codex/b"}},
	} {
		got, err := warmupOf(t, tc.settings)
		if err != nil || got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestMagpieWarmupFormat(t *testing.T) {
	for _, settings := range []string{`[]`, `{"codexWarmup":true}`, `{"claudeWarmup":"daily"}`, `{"codexWarmAts":"06:00"}`, `{"codexWarmAtOf":["a"]}`, `{`} {
		got, err := warmupOf(t, settings)
		if !errors.Is(err, host.ErrFormat) || got == nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v", settings, got, err)
		}
	}
	if got, err := (&Host{}).MagpieWarmup(warmAccounts); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no config dir: %v %v", got, err)
	}
}

// TestMagpieWarmupSeats matches a ChatGPT account's own time of day by
// Magpie's account name: a Team seat is "a@b.com · Team", apart from a
// personal plan of the same email.
func TestMagpieWarmupSeats(t *testing.T) {
	accounts := []host.Account{
		{Pin: "codex/personal", Provider: "codex", Email: "a@example.com", User: "a@example.com"},
		{Pin: "codex/team", Provider: "codex", Email: "a@example.com", User: "a@example.com · Team"},
	}
	dir := t.TempDir()
	h := &Host{ConfigDir: dir}
	for _, tc := range []struct {
		name, settings string
		want           []string
	}{
		{"seat only", `{"codexWarmAtOf":{"a@example.com · team":"07:30"}}`, []string{"codex/team"}},
		{"personal only", `{"codexWarmAtOf":{"a@example.com":"07:30"}}`, []string{"codex/personal"}},
		{"seat off", `{"codexWarmAts":["06:00"],"codexWarmAtOf":{"A@example.com · Team":"off"}}`, []string{"codex/personal"}},
		{"personal off", `{"codexWarmAts":["06:00"],"codexWarmAtOf":{"a@example.com":"off"}}`, []string{"codex/team"}},
	} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.settings), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := h.MagpieWarmup(accounts)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}
