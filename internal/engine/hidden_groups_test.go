package engine

import (
	"context"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// A hidden quota group is still queried, recorded and logged; only its
// notifications are dropped.
func TestHiddenGroupRecordsEventsAndHistoryWithoutNotifying(t *testing.T) {
	c := &clock{t: time.Unix(geminiReadings[0].at, 0).In(shanghai)}
	e, sender := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(context.Background(), e.config()); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cfg.HiddenGroups = []string{"antigravity|Gemini"}
	cred := e.creds["4"]
	for _, r := range geminiReadings {
		c.t = time.Unix(r.at, 0).In(shanghai)
		windows := []quota.Window{
			{ID: "gemini-5h", Label: quota.LabelFiveHour, Remaining: r.fiveHour, Reset: time.Unix(r.fiveHourResetUnix, 0)},
			{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: r.weekly, Reset: time.Unix(r.weeklyReset, 0)},
		}
		e.applyGroups(context.Background(), cfg, cred, []quota.RawGroup{{Key: "antigravity:gemini-models", SourceLabel: "Gemini", Windows: windows}}, quota.SourceActive, c.t, c.t, true)
	}
	var resets, notices int
	for _, event := range e.events {
		switch event.Kind {
		case "reset":
			resets++
		case "notify", "notify_failed":
			notices++
		}
	}
	if resets != 1 || notices != 0 || len(sender.sent) != 0 {
		t.Fatalf("want the reset event and no notification, got %d resets, %d notices, %+v", resets, notices, sender.sent)
	}
	ws := e.state.Groups["antigravity:4:antigravity:gemini-models"].Windows["gemini-weekly"]
	if ws == nil || ws.PendingRecovery != nil {
		t.Fatalf("the recovery of a hidden group is done with: %+v", ws)
	}
	records, err := e.history.Query(time.Unix(1794610000, 0), time.Unix(1794612000, 0))
	if err != nil {
		t.Fatal(err)
	}
	samples := 0
	for _, record := range records {
		if record.Kind == "s" && record.Window == "gemini-weekly" {
			samples++
		}
	}
	if samples != len(geminiReadings) {
		t.Fatalf("want every reading in the history, got %d", samples)
	}
}

// A reminder that came due while the group was hidden, with no reading in
// that time, is not sent by the first reading after it is shown again.
func TestShownGroupSkipsReminderDueWhileHidden(t *testing.T) {
	c := &clock{t: time.Unix(geminiReadings[0].at, 0).In(shanghai)}
	e, sender := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(context.Background(), e.config()); err != nil {
		t.Fatal(err)
	}
	cred := e.creds["4"]
	reset := c.t.Add(30 * time.Hour)
	read := func() {
		windows := []quota.Window{{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: 80, Reset: reset}}
		e.applyGroups(context.Background(), e.config(), cred, []quota.RawGroup{{Key: "antigravity:gemini-models", SourceLabel: "Gemini", Windows: windows}}, quota.SourceActive, c.t, c.t, true)
	}
	shown := e.config()
	hidden := shown
	hidden.HiddenGroups = []string{"antigravity|Gemini"}
	e.Configure(hidden, nil)
	read()
	// The 24-hour reminder comes due 6 hours later; the group is shown again
	// an hour after that, and read an hour later still.
	c.t = reset.Add(-23 * time.Hour)
	e.Configure(shown, nil)
	if span, ok := e.groupHiddenSpan("antigravity", "Gemini"); !ok || !span.to.Equal(c.t) || !span.from.Before(span.to) {
		t.Fatalf("want when the group was hidden and shown, got %+v", span)
	}
	if _, ok := e.groupHiddenSpan("antigravity", "Claude / GPT"); ok {
		t.Fatal("a group that was not hidden has no span")
	}
	c.t = reset.Add(-22 * time.Hour)
	read()
	if len(sender.sent) != 0 {
		t.Fatalf("the reminder due while hidden must not be sent: %+v", sender.sent)
	}
}

// A reminder due before the group was hidden, and not sent then for want
// of a reading, is sent by the first reading after it shows again.
func TestReminderDueBeforeHidingIsSent(t *testing.T) {
	c := &clock{t: time.Unix(geminiReadings[0].at, 0).In(shanghai)}
	e, sender := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(context.Background(), e.config()); err != nil {
		t.Fatal(err)
	}
	cred := e.creds["4"]
	reset := c.t.Add(25 * time.Hour)
	read := func() {
		windows := []quota.Window{{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: 80, Reset: reset}}
		e.applyGroups(context.Background(), e.config(), cred, []quota.RawGroup{{Key: "antigravity:gemini-models", SourceLabel: "Gemini", Windows: windows}}, quota.SourceActive, c.t, c.t, true)
	}
	shown := e.config()
	hidden := shown
	hidden.HiddenGroups = []string{"antigravity|Gemini"}
	read()
	// The 24-hour reminder comes due an hour later without a reading; the
	// group is hidden 10 minutes after that, shown again 5 minutes later,
	// and read 5 minutes later still.
	c.t = reset.Add(-23*time.Hour + -50*time.Minute)
	e.Configure(hidden, nil)
	c.t = c.t.Add(5 * time.Minute)
	e.Configure(shown, nil)
	c.t = c.t.Add(5 * time.Minute)
	read()
	if len(sender.sent) != 1 {
		t.Fatalf("want the reminder due before the group was hidden, got %+v", sender.sent)
	}
}

func TestHiddenSpans(t *testing.T) {
	at := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	e := &Engine{now: func() time.Time { return at }}
	// The groups of the first config are hidden since a time unknown.
	e.noteHiddenLocked(nil, []string{"a|A"}, true)
	if span := e.hidden["a|A"]; !span.from.IsZero() || !span.to.IsZero() {
		t.Fatalf("first config: %+v", span)
	}
	at = at.Add(time.Minute)
	e.noteHiddenLocked([]string{"a|A"}, []string{"b|B"}, false)
	if span := e.hidden["a|A"]; !span.from.IsZero() || !span.to.Equal(at) {
		t.Fatalf("shown again: %+v", span)
	}
	if span := e.hidden["b|B"]; !span.from.Equal(at) || !span.to.IsZero() {
		t.Fatalf("hidden: %+v", span)
	}
	// Hidden again, the group starts a new span.
	at = at.Add(time.Minute)
	e.noteHiddenLocked([]string{"b|B"}, []string{"a|A", "b|B"}, false)
	if span := e.hidden["a|A"]; !span.from.Equal(at) || !span.to.IsZero() {
		t.Fatalf("hidden again: %+v", span)
	}
	if span := e.hidden["b|B"]; !span.to.IsZero() {
		t.Fatalf("still hidden: %+v", span)
	}
}
