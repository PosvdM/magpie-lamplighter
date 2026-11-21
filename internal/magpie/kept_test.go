package magpie

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// keptJSON is a GET /v1/magpie/quotas?kept=1 answer in the shape of
// provider.Quota (internal/provider/quotas.go), with the window names
// Magpie gives Codex (quotaDurationName) and Claude (claudeKinds).
const keptJSON = `{"object":"list","data":[
 {"provider":"codex","name":"Codex","kind":"subscription","plan":"team","user":"A@example.com · Team","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"5 hours","used":12.5,"remaining":87.5,"resetsAt":"2026-11-21T10:00:00Z"},
   {"name":"7 days","used":40,"remaining":60,"resetsAt":"2026-11-25T00:00:00Z"},
   {"name":"30 days","used":5,"remaining":95,"resetsAt":"2026-12-15T00:00:00Z"},
   {"name":"Allowance","used":1,"remaining":99},
   {"name":"Image generation","used":0,"remaining":100,"text":true,"lane":"images","display":"Unknown"}]},
 {"provider":"claude","name":"Claude Code","kind":"subscription","user":"b@example.com","asOf":"2026-11-21T07:30:00Z","windows":[
   {"name":"5 hours","used":0,"remaining":100},
   {"name":"7 days","used":20,"remaining":80,"resetsAt":"2026-11-26T00:00:00Z"},
   {"name":"7 days · Opus","used":30,"remaining":70,"resetsAt":"2026-11-26T00:00:00Z"},
   {"name":"7 days · Fable","used":50,"remaining":50,"resetsAt":"2026-11-27T00:00:00Z"},
   {"name":"Extra usage","used":100,"remaining":0,"text":true,"display":"In use"}]},
 {"provider":"agy-cli","name":"Antigravity","kind":"subscription","user":"c@example.com","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"Gemini · 5 hours","used":10,"remaining":90,"resetsAt":"2026-11-21T10:00:00Z","pool":"Gemini"}]},
 {"provider":"codex","name":"Codex","kind":"subscription","user":"a@example.com · Team","from":"office","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"5 hours","used":99,"remaining":1,"resetsAt":"2026-11-21T10:00:00Z"}]},
 {"provider":"claude","name":"Claude Code","kind":"subscription","user":"nobody@example.com","readAt":"2026-11-21T07:58:00Z","windows":[
   {"name":"7 days","used":1,"remaining":99,"resetsAt":"2026-11-26T00:00:00Z"}]},
 {"provider":"deepseek","name":"DeepSeek","kind":"balance","balance":"¥10","windows":[]}]}`

func TestKeptQuotas(t *testing.T) {
	_, h := newGateway(t, map[string]string{
		"GET /v1/magpie/accounts": accountsJSON,
		"GET /v1/magpie/quotas":   keptJSON,
	})
	got, err := h.KeptQuotas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	want := []host.KeptQuota{
		{Pin: "codex/1a2b3c4d5e6f7a8b", ReadAt: at("2026-11-21T07:58:00Z"), Groups: []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{
			{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: 87.5, Reset: at("2026-11-21T10:00:00Z"), Period: quota.FiveHours},
			{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 60, Reset: at("2026-11-25T00:00:00Z"), Period: quota.SevenDays},
			{ID: "window-30d", Label: "30d", Remaining: 95, Reset: at("2026-12-15T00:00:00Z"), Period: 30 * 24 * time.Hour},
		}}}},
		{Pin: "claude/0f0e0d0c0b0a0908", ReadAt: at("2026-11-21T07:30:00Z"), Groups: []quota.RawGroup{
			{Key: "claude:main", SourceLabel: "Claude", Windows: []quota.Window{
				{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 80, Reset: at("2026-11-26T00:00:00Z"), Period: quota.SevenDays},
			}},
			{Key: "claude:seven-day-opus", SourceLabel: "Claude", Windows: []quota.Window{
				{ID: "seven-day-opus", Label: "7 Day Opus", Remaining: 70, Reset: at("2026-11-26T00:00:00Z"), Period: quota.SevenDays},
			}},
			{Key: providers.FableGroupKey, SourceLabel: "Fable", Windows: []quota.Window{
				{ID: quota.WindowFable, Label: quota.LabelFable, Remaining: 50, Reset: at("2026-11-27T00:00:00Z"), Period: quota.SevenDays},
			}},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept quotas:\n got %+v\nwant %+v", got, want)
	}
}

// The kept groups update the windows of the provider's own Fetch only
// when their keys, IDs and labels are the same.
func TestKeptMatchesFetch(t *testing.T) {
	reset := "2026-11-26T00:00:00Z"
	fetched, err := providers.ParseClaudeUsage(map[string]any{
		"five_hour":      map[string]any{"utilization": 10.0, "resets_at": reset},
		"seven_day":      map[string]any{"utilization": 20.0, "resets_at": reset},
		"seven_day_opus": map[string]any{"utilization": 30.0, "resets_at": reset},
		"limits": []any{map[string]any{"kind": "weekly_scoped", "percent": 50.0, "resets_at": reset,
			"scope": map[string]any{"model": map[string]any{"display_name": "Fable"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	used := func(v float64) *float64 { return &v }
	r, _ := time.Parse(time.RFC3339, reset)
	kept, err := keptClaude([]keptWindow{
		{Name: "5 hours", Used: used(10), ResetsAt: &r},
		{Name: "7 days", Used: used(20), ResetsAt: &r},
		{Name: "7 days · Opus", Used: used(30), ResetsAt: &r},
		{Name: "7 days · Fable", Used: used(50), ResetsAt: &r},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shape(kept), shape(fetched)) {
		t.Fatalf("Claude kept %v, fetched %v", shape(kept), shape(fetched))
	}
	fetched, err = providers.ParseCodexUsage(map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": 10.0, "limit_window_seconds": 18000.0, "reset_at": float64(r.Unix())},
		"secondary_window": map[string]any{"used_percent": 20.0, "limit_window_seconds": 2592000.0, "reset_at": float64(r.Unix())},
	}}, r)
	if err != nil {
		t.Fatal(err)
	}
	kept, err = keptCodex([]keptWindow{{Name: "5 hours", Used: used(10), ResetsAt: &r}, {Name: "30 days", Used: used(20), ResetsAt: &r}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shape(kept), shape(fetched)) {
		t.Fatalf("Codex kept %v, fetched %v", shape(kept), shape(fetched))
	}
}

// shape lists each group's key and source label, and each window's ID,
// label, period and remaining quota.
func shape(groups []quota.RawGroup) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.Key+"|"+g.SourceLabel)
		for _, w := range g.Windows {
			out = append(out, w.ID+"|"+w.Label+"|"+w.Period.String()+"|"+strconv.FormatFloat(w.Remaining, 'f', -1, 64)+"|"+w.Reset.UTC().Format(time.RFC3339))
		}
	}
	return out
}

func TestKeptQuotasFormat(t *testing.T) {
	for _, body := range []string{
		`{"object":"list"}`,
		`{"data":{}}`,
		`{"data":[{"provider":"codex","kind":"subscription","user":"a@example.com · Team","readAt":"2026-11-21T07:58:00Z","windows":[{"name":"5 hours","resetsAt":"2026-11-21T10:00:00Z"}]}]}`,
	} {
		_, h := newGateway(t, map[string]string{
			"GET /v1/magpie/accounts": accountsJSON,
			"GET /v1/magpie/quotas":   body,
		})
		if _, err := h.KeptQuotas(context.Background()); !errors.Is(err, host.ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", body, err)
		}
	}
}
