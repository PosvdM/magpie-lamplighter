package notify

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// sevenDayReset is the reset time of the 7-day window in these tests.
var sevenDayReset = time.Date(2026, 11, 25, 19, 59, 59, 0, time.UTC)

func sevenDayGroup(reset time.Time) Group {
	return Group{Key: "g", Label: "Claude", Windows: []quota.Window{
		{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 50, Reset: reset},
	}}
}

func reminderAlerts(hours []float64, sender Sender) *Alerts {
	cfg := config.Default()
	cfg.ResetReminder.SevenDay = config.ReminderAll
	if hours != nil {
		cfg.ResetReminderHours.SevenDay = hours
	}
	return alerts(cfg, sender)
}

func sevenDayNotices(st *store.State) map[string]string {
	return st.Groups["g"].Windows[quota.WindowSevenDay].ResetNotices
}

func TestReminderIgnoresDroppedSecondsOfReset(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := reminderAlerts(nil, sender)
	now := sevenDayReset.Add(-20 * time.Hour)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), now)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), now)
	if len(sender.sent) != 1 {
		t.Fatalf("want the reminder, got %+v", sender.sent)
	}
	// Claude Code's /usage reports the same reset without its seconds.
	truncated := sevenDayReset.Truncate(time.Minute)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(truncated), now.Add(5*time.Minute))
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), now.Add(10*time.Minute))
	a.ProcessGroup(context.Background(), st, sevenDayGroup(truncated), now.Add(15*time.Minute))
	if len(sender.sent) != 1 {
		t.Fatalf("the reminder must not be sent again for the same reset: %+v", sender.sent)
	}
}

func TestReminderPerLead(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := reminderAlerts([]float64{24, 10}, sender)
	for _, step := range []struct {
		before time.Duration
		want   int
	}{
		{30 * time.Hour, 0},
		{23 * time.Hour, 1},
		{20 * time.Hour, 1},
		{9 * time.Hour, 2},
		{time.Hour, 2},
	} {
		a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-step.before))
		if len(sender.sent) != step.want {
			t.Fatalf("%v before the reset: want %d reminders, got %+v", step.before, step.want, sender.sent)
		}
	}
	for _, msg := range sender.sent {
		if msg.Title != "⏰ Claude · 7d 重置提醒" {
			t.Fatalf("title %q", msg.Title)
		}
	}
	reset := formatReset(sevenDayReset)
	if got := sevenDayNotices(st); !maps.Equal(got, map[string]string{"24": reset, "10": reset}) {
		t.Fatalf("notices %v", got)
	}
}

func TestReminderForPassedLeadsIsSentOnce(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := reminderAlerts([]float64{24, 10}, sender)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-30*time.Hour))
	for _, before := range []time.Duration{9 * time.Hour, 8 * time.Hour, time.Hour} {
		a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-before))
	}
	if len(sender.sent) != 1 {
		t.Fatalf("want one reminder for both leads, got %+v", sender.sent)
	}
	reset := formatReset(sevenDayReset)
	if got := sevenDayNotices(st); !maps.Equal(got, map[string]string{"24": reset, "10": reset}) {
		t.Fatalf("notices %v", got)
	}
}

func TestUndeliveredReminderIsRetried(t *testing.T) {
	sender := &fakeSender{fail: true}
	st := store.NewState()
	a := reminderAlerts(nil, sender)
	now := sevenDayReset.Add(-20 * time.Hour)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), now)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), now)
	if len(sevenDayNotices(st)) != 0 {
		t.Fatalf("an undelivered reminder must not be recorded: %v", sevenDayNotices(st))
	}
	sender.fail = false
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), now.Add(5*time.Minute))
	if len(sender.sent) != 1 {
		t.Fatalf("want the retried reminder, got %+v", sender.sent)
	}
}

func TestLegacyReminderFieldsAreNotSentAgain(t *testing.T) {
	reset := formatReset(sevenDayReset)
	fiveHourReset := sevenDayReset.Add(-20*time.Hour + 30*time.Minute)
	raw := `{"version":1,"groups":{"g":{"active_windows":null,"windows":{` +
		`"` + quota.WindowSevenDay + `":{"remaining":80,"reset":"` + reset + `","reset_notice_1d_for":"` + reset + `"},` +
		`"` + quota.WindowFiveHour + `":{"remaining":80,"reset":"` + formatReset(fiveHourReset) + `","reset_notice_1h_for":"` + formatReset(fiveHourReset) + `"}` +
		`}}}}`
	st := store.NewState()
	if err := json.Unmarshal([]byte(raw), st); err != nil {
		t.Fatal(err)
	}
	if got := sevenDayNotices(st); !maps.Equal(got, map[string]string{"24": reset}) {
		t.Fatalf("7d notices %v", got)
	}
	if got := st.Groups["g"].Windows[quota.WindowFiveHour].ResetNotices; !maps.Equal(got, map[string]string{"1": formatReset(fiveHourReset)}) {
		t.Fatalf("5h notices %v", got)
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "reset_notice_1") {
		t.Fatalf("legacy keys must not be written: %s", out)
	}

	sender := &fakeSender{}
	cfg := config.Default()
	cfg.ResetReminder = config.WindowModes{FiveHour: config.ReminderAll, SevenDay: config.ReminderAll}
	now := sevenDayReset.Add(-20 * time.Hour)
	g := Group{Key: "g", Label: "Claude", Windows: []quota.Window{
		{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: 80, Reset: fiveHourReset},
		{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: 80, Reset: sevenDayReset},
	}}
	alerts(cfg, sender).ProcessGroup(context.Background(), st, g, now)
	if len(sender.sent) != 0 {
		t.Fatalf("reminders sent before the upgrade must not be sent again: %+v", sender.sent)
	}
}

func TestRebaseKeepsReminderNotices(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := reminderAlerts([]float64{24, 10}, sender)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-30*time.Hour))
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-20*time.Hour))
	if len(sender.sent) != 1 {
		t.Fatalf("want the 24-hour reminder, got %+v", sender.sent)
	}
	st.Groups["g"].Windows[quota.WindowSevenDay].Rebase = true
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-19*time.Hour))
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-18*time.Hour))
	if got := sevenDayNotices(st); len(sender.sent) != 1 || got["24"] != formatReset(sevenDayReset) {
		t.Fatalf("sent %+v, notices %v", sender.sent, got)
	}
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-9*time.Hour))
	if len(sender.sent) != 2 {
		t.Fatalf("want the 10-hour reminder, got %+v", sender.sent)
	}
}

func TestReminderNoticesOfEarlierCyclesAreDropped(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := reminderAlerts([]float64{24, 10}, sender)
	previous := sevenDayReset.Add(-7 * 24 * time.Hour)
	a.ProcessGroup(context.Background(), st, sevenDayGroup(previous), previous.Add(-30*time.Hour))
	a.ProcessGroup(context.Background(), st, sevenDayGroup(previous), previous.Add(-9*time.Hour))
	a.ProcessGroup(context.Background(), st, sevenDayGroup(sevenDayReset), sevenDayReset.Add(-20*time.Hour))
	reminders := 0
	for _, msg := range sender.sent {
		if msg.Kind == KindReminder {
			reminders++
		}
	}
	if reminders != 2 {
		t.Fatalf("want one reminder per cycle, got %+v", sender.sent)
	}
	if got := sevenDayNotices(st); !maps.Equal(got, map[string]string{"24": formatReset(sevenDayReset)}) {
		t.Fatalf("notices %v", got)
	}
}
