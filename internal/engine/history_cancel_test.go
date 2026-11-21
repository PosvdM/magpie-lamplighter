package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// cancelAfter is a context that is cancelled after its Err has been asked
// n times, like a request whose client goes away during the scan.
type cancelAfter struct {
	context.Context
	n int
}

func (c *cancelAfter) Err() error {
	if c.n--; c.n < 0 {
		return context.Canceled
	}
	return nil
}

// GET /api/history scans the history files with the request's context:
// the scan ends when the request does, also halfway through.
func TestHistoryRangeEndsWithTheRequest(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	e, _ := newTestEngine(t, standardHost(c), c)
	batch := make([]store.Record, 0, 2000)
	for i := 0; i < cap(batch); i++ {
		batch = append(batch, store.Record{Kind: "s", Time: c.t.Add(-time.Duration(i) * time.Second).Unix(), Group: "g", Window: "five-hour", Remaining: 50})
	}
	if err := e.history.Append(batch...); err != nil {
		t.Fatal(err)
	}
	if resp, err := e.HistoryRange(context.Background(), "1h", ""); err != nil || len(resp.Series) != 1 {
		t.Fatalf("history %+v, %v", resp, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.HistoryRange(ctx, "1h", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: err = %v", err)
	}
	if _, err := e.HistoryRange(&cancelAfter{Context: context.Background(), n: 100}, "1h", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("request cancelled during the scan: err = %v", err)
	}
}
