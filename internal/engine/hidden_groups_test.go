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
