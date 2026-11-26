package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

func TestHiddenGroupSendsNothingAndLeavesNothingToCatchUp(t *testing.T) {
	cfg := config.Default()
	cfg.RecoveryNotify.FiveHour = config.RecoveryAll
	cfg.ResetReminder.FiveHour = config.ReminderAll
	sender := &fakeSender{}
	st := store.NewState()
	a := alerts(cfg, sender)
	start := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	reset := start.Add(4 * time.Hour)
	hidden := func(g Group) Group {
		g.Hidden = true
		return g
	}
	a.ProcessGroup(context.Background(), st, fiveHour(80, reset), start)
	// Hidden: a threshold, the exhaustion and the reset reminder.
	a.ProcessGroup(context.Background(), st, hidden(fiveHour(30, reset)), start.Add(time.Hour))
	a.ProcessGroup(context.Background(), st, hidden(fiveHour(0, reset)), reset.Add(-30*time.Minute))
	// Shown again in the same cycle: what was due while hidden is not sent.
	a.ProcessGroup(context.Background(), st, fiveHour(0, reset), reset.Add(-20*time.Minute))
	// Hidden over the reset, then shown in the new cycle.
	after := reset.Add(time.Minute)
	next := after.Add(5 * time.Hour)
	a.ProcessGroup(context.Background(), st, hidden(fiveHour(100, next)), after)
	a.ProcessGroup(context.Background(), st, fiveHour(100, next), after.Add(5*time.Minute))
	if len(sender.sent) != 0 {
		t.Fatalf("a hidden group sends nothing, then or once shown: %+v", sender.sent)
	}
	ws := st.Groups["g"].Windows["five-hour"]
	if ws.PendingRecovery != nil || ws.NotifiedSeverity != SeverityNormal {
		t.Fatalf("the baseline must follow the hidden readings: %+v", ws)
	}
	// Once shown, the group notifies as before.
	a.ProcessGroup(context.Background(), st, fiveHour(40, next), after.Add(time.Hour))
	if len(sender.sent) != 1 || !strings.HasPrefix(sender.sent[0].Title, "🟡 Claude") {
		t.Fatalf("want the threshold alert of the shown group, got %+v", sender.sent)
	}
}

func TestHiddenGroupNeedsNoSender(t *testing.T) {
	st := store.NewState()
	a := alerts(config.Default(), nil)
	now := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	a.ProcessGroup(context.Background(), st, fiveHour(80, now.Add(time.Hour)), now)
	g := fiveHour(10, now.Add(time.Hour))
	g.Hidden = true
	a.ProcessGroup(context.Background(), st, g, now.Add(time.Minute))
	if got := st.Groups["g"].Windows["five-hour"].NotifiedSeverity; got != SeverityCritical {
		t.Fatalf("the hidden alert counts as done, got %q", got)
	}
}

func TestShownGroupSkipsRemindersDueWhileHidden(t *testing.T) {
	cfg := config.Default()
	cfg.ResetReminder.FiveHour = config.ReminderAll
	cfg.ResetReminderHours.FiveHour = []float64{1, 0.5}
	sender := &fakeSender{}
	st := store.NewState()
	a := alerts(cfg, sender)
	reset := time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)
	g := fiveHour(80, reset)
	g.Hidden = true
	a.ProcessGroup(context.Background(), st, g, reset.Add(-3*time.Hour))
	a.ProcessGroup(context.Background(), st, g, reset.Add(-2*time.Hour))
	// The 1-hour reminder comes due at 04:00 with no reading; the group is
	// shown again at 04:10, and the next reading comes at 04:15.
	g.Hidden = false
	g.Shown = reset.Add(-50 * time.Minute)
	a.ProcessGroup(context.Background(), st, g, reset.Add(-45*time.Minute))
	if len(sender.sent) != 0 {
		t.Fatalf("the reminder due while hidden must not be sent: %+v", sender.sent)
	}
	if got := st.Groups["g"].Windows["five-hour"].ResetNotices["1"]; got != formatReset(reset) {
		t.Fatalf("the reminder due while hidden counts as sent, got %q", got)
	}
	// The half-hour reminder comes due after the group was shown.
	a.ProcessGroup(context.Background(), st, g, reset.Add(-25*time.Minute))
	if len(sender.sent) != 1 || !strings.HasPrefix(sender.sent[0].Title, "⏰") {
		t.Fatalf("want the reminder due after the group was shown, got %+v", sender.sent)
	}
	// In the next cycle the time it was shown holds nothing back.
	next := reset.Add(5 * time.Hour)
	g.Windows = fiveHour(80, next).Windows
	a.ProcessGroup(context.Background(), st, g, reset.Add(time.Minute))
	a.ProcessGroup(context.Background(), st, g, next.Add(-55*time.Minute))
	if len(sender.sent) != 2 {
		t.Fatalf("want the reminder of the next cycle, got %+v", sender.sent)
	}
}

// Only the reminders that came due while the group was hidden are held
// back; one due before it was hidden and not sent yet is sent once it shows.
func TestReminderDueBeforeHidingIsSentOnceShown(t *testing.T) {
	cfg := config.Default()
	cfg.ResetReminder.FiveHour = config.ReminderAll
	cfg.ResetReminderHours.FiveHour = []float64{1, 0.5}
	sender := &fakeSender{}
	st := store.NewState()
	a := alerts(cfg, sender)
	reset := time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 26, h, m, 0, 0, time.UTC) }
	g := fiveHour(80, reset)
	a.ProcessGroup(context.Background(), st, g, at(3, 50))
	// The 1-hour reminder comes due at 04:00 with no reading; the group is
	// hidden at 04:05, read at 04:07 and shown again at 04:10.
	g.Hidden, g.HiddenSince = true, at(4, 5)
	a.ProcessGroup(context.Background(), st, g, at(4, 7))
	if len(sender.sent) != 0 {
		t.Fatalf("a hidden group sends nothing: %+v", sender.sent)
	}
	g.Hidden, g.Shown = false, at(4, 10)
	a.ProcessGroup(context.Background(), st, g, at(4, 15))
	if len(sender.sent) != 1 || !strings.HasPrefix(sender.sent[0].Title, "⏰") {
		t.Fatalf("want the reminder due before the group was hidden, got %+v", sender.sent)
	}
	// Hidden from 04:20 to 04:40: the half-hour reminder, due at 04:30, is
	// not sent, with or without a reading in that time.
	g.HiddenSince, g.Shown = at(4, 20), at(4, 40)
	a.ProcessGroup(context.Background(), st, g, at(4, 45))
	if len(sender.sent) != 1 {
		t.Fatalf("the reminder due while hidden must not be sent: %+v", sender.sent)
	}
	if got := st.Groups["g"].Windows["five-hour"].ResetNotices["0.5"]; got != formatReset(reset) {
		t.Fatalf("the reminder due while hidden counts as sent, got %q", got)
	}
}
