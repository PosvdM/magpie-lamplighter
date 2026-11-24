package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResetReminderHours(t *testing.T) {
	defaults := WindowHours{FiveHour: []float64{1}, SevenDay: []float64{24}}
	for _, tc := range []struct {
		name, raw string
		want      WindowHours
	}{
		{"absent", "", defaults},
		{"partial", "reset_reminder_hours:\n  seven_day: [24, 10]\n", WindowHours{FiveHour: []float64{1}, SevenDay: []float64{24, 10}}},
		{"sorted", "reset_reminder_hours:\n  five_hour: [1, 2]\n  seven_day: [10, 48, 24]\n", WindowHours{FiveHour: []float64{2, 1}, SevenDay: []float64{48, 24, 10}}},
		{"range", "reset_reminder_hours:\n  five_hour: [0.1, 5]\n  seven_day: [0, 200]\n", WindowHours{FiveHour: []float64{4.5, 0.5}, SevenDay: []float64{167, 1}}},
		{"duplicates", "reset_reminder_hours:\n  five_hour: [1, 1, 1.0]\n  seven_day: [200, 168, 24]\n", WindowHours{FiveHour: []float64{1}, SevenDay: []float64{167, 24}}},
		{"at most three", "reset_reminder_hours:\n  seven_day: [1, 2, 3, 4, 5]\n", WindowHours{FiveHour: []float64{1}, SevenDay: []float64{5, 4, 3}}},
		{"not finite", "reset_reminder_hours:\n  five_hour: [.nan, .inf, 2]\n  seven_day: [-.inf]\n", WindowHours{FiveHour: []float64{2}, SevenDay: []float64{24}}},
		{"empty", "reset_reminder_hours:\n  five_hour: []\n  seven_day: null\n", defaults},
		{"json", `{"reset_reminder_hours": {"seven_day": [12.5, 72]}}`, WindowHours{FiveHour: []float64{1}, SevenDay: []float64{72, 12.5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.ResetReminderHours, tc.want) {
				t.Fatalf("got %+v, want %+v", cfg.ResetReminderHours, tc.want)
			}
		})
	}
	cfg := Default()
	if got := cfg.ResetReminderHours.For(true); !reflect.DeepEqual(got, []float64{24}) {
		t.Fatalf("7d hours %v", got)
	}
	if got := cfg.ResetReminderHours.For(false); !reflect.DeepEqual(got, []float64{1}) {
		t.Fatalf("5h hours %v", got)
	}
	raw, err := json.Marshal(cfg.ResetReminderHours)
	if err != nil || string(raw) != `{"five_hour":[1],"seven_day":[24]}` {
		t.Fatalf("JSON %s, %v", raw, err)
	}
}
