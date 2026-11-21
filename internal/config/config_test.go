package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseEmptyUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 300 || !cfg.Ignition.Enabled || cfg.Ignition.StartHour != 7 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
	if !cfg.Provider("codex").Ignite || cfg.Provider("antigravity").Ignite {
		t.Fatal("codex ignites by default, antigravity does not")
	}
	if cfg.RememberChartFilters {
		t.Fatal("chart filters are not remembered by default")
	}
}

func TestParseRememberChartFilters(t *testing.T) {
	cfg, err := Parse([]byte("remember_chart_filters: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RememberChartFilters {
		t.Fatal("remember_chart_filters not read")
	}
}

func TestPartialProviderBlockKeepsDefaults(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\npriority: 1\nproviders:\n  antigravity:\n    ignite: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	ag := cfg.Provider("antigravity")
	if !ag.Ignite || !ag.Monitor || len(ag.Groups) != 1 || ag.Groups[0] != "Gemini" {
		t.Fatalf("antigravity %+v", ag)
	}
	if !cfg.Provider("claude").Ignite {
		t.Fatal("other providers keep their defaults")
	}
}

func TestParseClampsValues(t *testing.T) {
	cfg, err := Parse([]byte("poll_interval_seconds: 5\nignition:\n  start_hour: 30\n  max_transient_failures: 50\ncodex_reset_updates:\n  poll_seconds: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 60 || cfg.Ignition.StartHour != 23 || cfg.Ignition.MaxTransientFailures != 10 || cfg.CodexResetUpdates.PollSeconds != 300 {
		t.Fatalf("not clamped: %+v", cfg)
	}
	if cfg.Ignition.EndHour != 22 {
		t.Fatal("keys missing from a partial block keep their defaults")
	}
}

func TestParseRejectsInvalidYAML(t *testing.T) {
	if _, err := Parse([]byte("bark_url: [")); err == nil {
		t.Fatal("invalid YAML must fail")
	}
}

func TestNotificationModes(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryAfterExhausted, SevenDay: RecoveryAll}) ||
		cfg.ResetReminder != (WindowModes{FiveHour: ReminderOff, SevenDay: ReminderHasRemaining}) {
		t.Fatalf("default modes: %+v %+v", cfg.RecoveryNotify, cfg.ResetReminder)
	}
	cfg, err = Parse([]byte("recovery_notify:\n  five_hour: after_exhausted\n  seven_day: bogus\nreset_reminder:\n  seven_day: has_remaining\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryAfterExhausted, SevenDay: RecoveryOff}) {
		t.Fatalf("recovery_notify %+v", cfg.RecoveryNotify)
	}
	// A key missing from a partial block keeps its default.
	if cfg.ResetReminder != (WindowModes{FiveHour: ReminderOff, SevenDay: ReminderHasRemaining}) {
		t.Fatalf("reset_reminder %+v", cfg.ResetReminder)
	}
	cfg, err = Parse([]byte("reset_reminder:\n  five_hour: all\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResetReminder != (WindowModes{FiveHour: ReminderAll, SevenDay: ReminderHasRemaining}) {
		t.Fatalf("partial reset_reminder %+v", cfg.ResetReminder)
	}
}

func TestStorePatchesTopLevelKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", FileName)
	store, cfg, err := Open(path)
	if err != nil || cfg.PollIntervalSeconds != 300 {
		t.Fatalf("missing file must give defaults: %+v %v", cfg, err)
	}
	if _, err := store.Patch(map[string]json.RawMessage{"bark_url": json.RawMessage(`"https://b/k/"`), "poll_interval_seconds": json.RawMessage(`600`)}); err != nil {
		t.Fatal(err)
	}
	cfg, err = store.Patch(map[string]json.RawMessage{"poll_interval_seconds": json.RawMessage(`null`)})
	if err != nil || cfg.BarkURL != "https://b/k" || cfg.PollIntervalSeconds != 300 {
		t.Fatalf("cfg %+v err %v", cfg, err)
	}
	if _, err := store.Patch(map[string]json.RawMessage{"ignition": json.RawMessage(`[`)}); err == nil {
		t.Fatal("a patch that does not parse must fail")
	}
	_, cfg, err = Open(path)
	if err != nil || cfg.BarkURL != "https://b/k" {
		t.Fatalf("reopened %+v %v", cfg, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode %v %v", info, err)
	}
}

func TestDataDir(t *testing.T) {
	t.Setenv("LAMPLIGHTER_DATA_DIR", "/x/data")
	if got, _ := DataDir(); got != "/x/data" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("LAMPLIGHTER_DATA_DIR", "")
	t.Setenv("XDG_STATE_HOME", "/config/state")
	if got, _ := DataDir(); got != filepath.Join("/config/state", "lamplighter") {
		t.Fatalf("got %q", got)
	}
}

func TestParseAccountWeightsDropsInvalidWeights(t *testing.T) {
	cfg, err := Parse([]byte("account_weights:\n  a.json: 5\n  b.json: 0.5\n  c.json: 0\n  d.json: -2\n  e.json: .inf\n  f.json: .nan\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AccountWeights) != 2 || cfg.AccountWeights["a.json"] != 5 || cfg.AccountWeights["b.json"] != 0.5 {
		t.Fatalf("account weights %v", cfg.AccountWeights)
	}
}
