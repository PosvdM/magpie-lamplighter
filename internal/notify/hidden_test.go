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
