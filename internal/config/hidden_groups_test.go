package config

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHiddenGroups(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"absent", "", nil},
		{"yaml", "hidden_groups:\n  - antigravity|Claude / GPT\n", []string{"antigravity|Claude / GPT"}},
		{"json", `{"hidden_groups": ["claude|Fable", "antigravity|Claude / GPT"]}`, []string{"claude|Fable", "antigravity|Claude / GPT"}},
		{"trimmed", `{"hidden_groups": [" claude|Fable ", "", "  ", "claude|Fable"]}`, []string{"claude|Fable"}},
		{"empty", `{"hidden_groups": []}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.HiddenGroups, tc.want) {
				t.Fatalf("got %#v, want %#v", cfg.HiddenGroups, tc.want)
			}
		})
	}
	cfg, _ := Parse([]byte(`{"hidden_groups": ["antigravity|Claude / GPT"]}`))
	if !cfg.GroupHidden("antigravity", "Claude / GPT") {
		t.Fatal("the listed group is hidden")
	}
	for _, other := range [][2]string{{"antigravity", "Gemini"}, {"claude", "Claude / GPT"}, {"antigravity", "claude / gpt"}, {"antigravity", ""}} {
		if cfg.GroupHidden(other[0], other[1]) {
			t.Fatalf("%v is not hidden", other)
		}
	}
	if _, err := Parse([]byte(`{"hidden_groups": "antigravity|Claude / GPT"}`)); err == nil {
		t.Fatal("hidden_groups must be a list")
	}
}

func TestHiddenGroupsAreWrittenOnlyWhenSet(t *testing.T) {
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"hidden_groups"`)) {
		t.Fatalf("default config names hidden_groups: %s", raw)
	}
	store, _, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.Patch(map[string]json.RawMessage{"hidden_groups": json.RawMessage(`["antigravity|Claude / GPT"]`)})
	if err != nil || !cfg.GroupHidden("antigravity", "Claude / GPT") {
		t.Fatalf("patch %+v %v", cfg, err)
	}
	if IsPageState(map[string]json.RawMessage{"hidden_groups": json.RawMessage(`[]`)}) {
		t.Fatal("hidden_groups changes the notifications, so the plugin applies it")
	}
	cfg, err = store.Patch(map[string]json.RawMessage{"hidden_groups": json.RawMessage(`null`)})
	if err != nil || cfg.HiddenGroups != nil {
		t.Fatalf("delete %+v %v", cfg, err)
	}
	if _, ok := store.Raw()["hidden_groups"]; ok {
		t.Fatalf("hidden_groups still saved: %s", store.Raw())
	}
}
