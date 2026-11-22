package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestParseEmptyUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 300 || cfg.Ignition.Enabled || cfg.Ignition.StartHour != 7 {
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

func TestParsePageState(t *testing.T) {
	cfg, err := Parse([]byte(`{"remember_chart_expanded": true, "chart_view": {"window": "7d", "sort": "low", "ranges": {"5h": "12h", "7d": "15d"}},
		"chart_expanded": ["claude|Claude", "codex|GPT"], "mcp_public_url": "https://mcp.example.com", "mask_accounts": true, "events_show_5h_resets": true}`))
	if err != nil {
		t.Fatal(err)
	}
	want := ChartView{Window: "7d", Sort: "low", Ranges: map[string]string{"5h": "12h", "7d": "15d"}}
	if !cfg.RememberChartExpanded || cfg.ChartView == nil || !reflect.DeepEqual(*cfg.ChartView, want) ||
		!reflect.DeepEqual(cfg.ChartExpanded, []string{"claude|Claude", "codex|GPT"}) || cfg.MCPPublicURL != "https://mcp.example.com" ||
		!cfg.MaskAccounts || !cfg.EventsShow5hResets {
		t.Fatalf("page state %+v", cfg)
	}
	// The status leaves out the keys the file does not set.
	cfg, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"remember_chart_expanded", "chart_view", "chart_expanded", "mcp_public_url", "mask_accounts", "events_show_5h_resets"} {
		if bytes.Contains(raw, []byte(`"`+key+`"`)) {
			t.Fatalf("default config names %s: %s", key, raw)
		}
	}
}

func TestStorePatchesPageState(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	view := `{"window":"5h","sort":"default","ranges":{"5h":"6h","7d":"8d"}}`
	cfg, err := store.Patch(map[string]json.RawMessage{
		"remember_chart_filters":  json.RawMessage(`true`),
		"remember_chart_expanded": json.RawMessage(`true`),
		"chart_view":              json.RawMessage(view),
		"chart_expanded":          json.RawMessage(`["claude|Claude"]`),
		"mask_accounts":           json.RawMessage(`true`),
	})
	if err != nil || cfg.ChartView == nil || cfg.ChartView.Ranges["7d"] != "8d" || len(cfg.ChartExpanded) != 1 || !cfg.MaskAccounts {
		t.Fatalf("patch %+v %v", cfg, err)
	}
	if string(store.Raw()["chart_view"]) != view || string(store.Raw()["chart_expanded"]) != `["claude|Claude"]` {
		t.Fatalf("raw %s", store.Raw())
	}
	_, cfg, err = Open(path)
	if err != nil || !cfg.RememberChartExpanded || cfg.ChartView == nil || cfg.ChartView.Window != "5h" || cfg.ChartExpanded[0] != "claude|Claude" {
		t.Fatalf("reopened %+v %v", cfg, err)
	}
	cfg, err = store.Patch(map[string]json.RawMessage{"chart_view": json.RawMessage(`null`), "chart_expanded": json.RawMessage(`null`), "mask_accounts": json.RawMessage(`null`)})
	if err != nil || cfg.ChartView != nil || cfg.ChartExpanded != nil || cfg.MaskAccounts {
		t.Fatalf("delete %+v %v", cfg, err)
	}
	raw := store.Raw()
	for _, key := range []string{"chart_view", "chart_expanded", "mask_accounts"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("%s still saved: %s", key, raw)
		}
	}
	if string(raw["remember_chart_filters"]) != "true" {
		t.Fatalf("other keys must stay: %s", raw)
	}
}

// The state of a remember switch is not kept while the switch is off,
// whoever writes it.
func TestStoreDropsTheStateOfASwitchThatIsOff(t *testing.T) {
	store, _, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	view := json.RawMessage(`{"window":"7d"}`)
	cfg, err := store.Patch(map[string]json.RawMessage{"chart_view": view, "chart_expanded": json.RawMessage(`["claude|Claude"]`)})
	if err != nil || cfg.ChartView != nil || cfg.ChartExpanded != nil {
		t.Fatalf("state saved with the switches off: %+v %v", cfg, err)
	}
	if raw := store.Raw(); raw["chart_view"] != nil || raw["chart_expanded"] != nil {
		t.Fatalf("raw %s", raw)
	}
	// A state the switch would not keep is dropped before it is parsed, so
	// a value of the wrong type does not fail the patch; with the switch on
	// it does.
	bad := json.RawMessage(`{"claude|Claude": true}`)
	if cfg, err := store.Patch(map[string]json.RawMessage{"chart_expanded": bad, "chart_view": json.RawMessage(`"7d"`)}); err != nil || cfg.ChartExpanded != nil {
		t.Fatalf("wrong type with the switch off: %+v %v", cfg, err)
	}
	if _, err := store.Patch(map[string]json.RawMessage{"remember_chart_expanded": json.RawMessage(`true`), "chart_expanded": bad}); err == nil {
		t.Fatal("wrong type with the switch on saved")
	}
	cfg, err = store.Patch(map[string]json.RawMessage{"remember_chart_filters": json.RawMessage(`true`), "chart_view": view,
		"remember_chart_expanded": json.RawMessage(`true`), "chart_expanded": json.RawMessage(`["claude|Claude"]`)})
	if err != nil || cfg.ChartView == nil || len(cfg.ChartExpanded) != 1 {
		t.Fatalf("switches on: %+v %v", cfg, err)
	}
	// Turning a switch off deletes its state even when the patch leaves the
	// state out.
	cfg, err = store.Patch(map[string]json.RawMessage{"remember_chart_filters": json.RawMessage(`null`)})
	if err != nil || cfg.ChartView != nil || store.Raw()["chart_view"] != nil || len(cfg.ChartExpanded) != 1 {
		t.Fatalf("switch off: %+v %v %s", cfg, err, store.Raw())
	}
}

// The page's publicUrl is tested with the same cases, so both accept the
// same addresses.
func TestMCPPublicURLValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "public_urls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]*string
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		// Escaped, since YAML, which Parse also reads, refuses some
		// control characters in its input.
		value := `"`
		for _, u := range utf16.Encode([]rune(*c[0])) {
			value += fmt.Sprintf(`\u%04x`, u)
		}
		value += `"`
		cfg, err := Parse([]byte(`{"mcp_public_url": ` + value + `}`))
		if err != nil {
			t.Fatalf("%q: %v", *c[0], err)
		}
		if got, want := cfg.Check() == nil, c[1] != nil; got != want {
			t.Errorf("%q: valid %v, want %v", *c[0], got, want)
		}
	}
	store, _, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Patch(map[string]json.RawMessage{"mcp_public_url": json.RawMessage(`"ftp://x.com"`)}); !errors.Is(err, ErrMCPPublicURL) {
		t.Fatalf("patch: %v", err)
	}
	if cfg, err := store.Patch(map[string]json.RawMessage{"mcp_public_url": json.RawMessage(`" https://x.com "`)}); err != nil || cfg.MCPPublicURL != "https://x.com" {
		t.Fatalf("patch: %+v %v", cfg, err)
	}
}

func TestIsPageState(t *testing.T) {
	raw := func(keys ...string) map[string]json.RawMessage {
		patch := map[string]json.RawMessage{}
		for _, key := range keys {
			patch[key] = json.RawMessage(`null`)
		}
		return patch
	}
	if !IsPageState(raw("chart_view", "remember_chart_filters", "chart_expanded", "remember_chart_expanded", "mask_accounts", "events_show_5h_resets", "mcp_public_url")) {
		t.Fatal("page keys")
	}
	for _, patch := range []map[string]json.RawMessage{raw(), raw("chart_view", "config_token"), raw("mask_accounts", "bark_url")} {
		if IsPageState(patch) {
			t.Fatalf("%v is not page state", patch)
		}
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

// Ignition is off on a new installation and when a file leaves out
// ignition.enabled; a file that sets it keeps its value.
func TestIgnitionEnabledDefault(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"ignition:\n  start_hour: 8\n", false},
		{"ignition:\n  enabled: true\n", true},
		{`{"ignition":{"enabled":true,"start_hour":8}}`, true},
		{`{"ignition":{"enabled":false}}`, false},
	} {
		cfg, err := Parse([]byte(tc.raw))
		if err != nil || cfg.Ignition.Enabled != tc.want {
			t.Fatalf("%q: enabled=%v err=%v", tc.raw, cfg.Ignition.Enabled, err)
		}
	}
	_, cfg, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil || cfg.Ignition.Enabled {
		t.Fatalf("missing file: enabled=%v err=%v", cfg.Ignition.Enabled, err)
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{"", nil},
		{"admin_key: a\nmcp_key: b\n", nil},
		{"admin_key: ''\nmcp_key: ''\n", nil},
		{"admin_key: k\nmcp_key: k\n", ErrSameKeys},
		{"admin_key: ' k '\nmcp_key: k\n", ErrSameKeys},
		{"timezone: Asia/Shanghai\n", nil},
		{"timezone: UTC\n", nil},
		{"timezone: ' '\n", nil},
		{"timezone: Mars/Olympus\n", ErrTimezone},
		{"timezone: ../../etc/passwd\n", ErrTimezone},
		// time.LoadLocation takes "Local" as the process time zone, which
		// an empty timezone already means.
		{"timezone: Local\n", ErrTimezone},
	} {
		cfg, err := Parse([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Check(); !errors.Is(got, tc.want) {
			t.Fatalf("%q: %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestPatchRefusesSameKeysAndInvalidTimezone(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Patch(map[string]json.RawMessage{"admin_key": json.RawMessage(`"secret"`)}); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []map[string]json.RawMessage{
		{"mcp_key": json.RawMessage(`" secret "`)},
		{"admin_key": json.RawMessage(`"x"`), "mcp_key": json.RawMessage(`"x"`)},
		{"timezone": json.RawMessage(`"Not/AZone"`)},
	} {
		_, err := store.Patch(patch)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "Not/AZone") {
			t.Fatalf("%s: %v", patch, err)
		}
	}
	raw := store.Raw()
	if len(raw) != 1 || string(raw["admin_key"]) != `"secret"` {
		t.Fatalf("a refused patch changed the store: %v", raw)
	}
	_, cfg, err := Open(path)
	if err != nil || cfg.AdminKey != "secret" || cfg.MCPKey != "" || cfg.Timezone != "" {
		t.Fatalf("a refused patch reached the file: %+v %v", cfg, err)
	}
}

// The settings page saves an empty time zone as "": no timezone, which
// Patch accepts.
func TestPatchAcceptsEmptyTimezone(t *testing.T) {
	store, _, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Patch(map[string]json.RawMessage{"timezone": json.RawMessage(`"Asia/Shanghai"`)}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`""`, `" "`} {
		cfg, err := store.Patch(map[string]json.RawMessage{"timezone": json.RawMessage(value)})
		if err != nil || cfg.Timezone != "" {
			t.Fatalf("timezone %s: %+v %v", value, cfg.Timezone, err)
		}
	}
}

// A file written by hand may hold what Patch refuses. Open keeps it: an
// invalid timezone falls back to the process time zone with a warning.
func TestOpenKeepsCheckedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(`{"timezone":"Mars/Olympus","admin_key":"k","mcp_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	store, cfg, err := Open(path)
	if err != nil || store == nil {
		t.Fatal(err)
	}
	if !cfg.SameKeys() || cfg.Location() != time.Local {
		t.Fatalf("cfg %+v location %v", cfg, cfg.Location())
	}
	if !strings.Contains(logged.String(), "timezone") || strings.Contains(logged.String(), "Mars") {
		t.Fatalf("warning %q", logged.String())
	}
}

// A hand-written "Local" is an invalid timezone: Open warns, and Location
// falls back to timezone_offset_hours, then the process time zone.
func TestOpenTreatsLocalTimezoneAsInvalid(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, tc := range []struct {
		file   string
		offset int
	}{
		{`{"timezone":"Local","timezone_offset_hours":8}`, 8 * 3600},
		{`{"timezone":"Local"}`, -1},
	} {
		logged.Reset()
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		_, cfg, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logged.String(), "timezone") {
			t.Fatalf("%s: no warning: %q", tc.file, logged.String())
		}
		loc := cfg.Location()
		if tc.offset < 0 {
			if loc != time.Local {
				t.Fatalf("%s: location %v", tc.file, loc)
			}
			continue
		}
		if _, offset := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(loc).Zone(); offset != tc.offset || loc == time.Local {
			t.Fatalf("%s: location %v offset %d", tc.file, loc, offset)
		}
	}
}
