package engine

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/failure"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
)

// ledgerHost is a gateway that keeps track of the streams it opened and
// not yet closed. open decides the response of each request and may wait
// on its context; read returns the next chunk of a stream and may wait
// until the stream is closed.
type ledgerHost struct {
	*fakeHost
	open    func(ctx context.Context) (host.ModelStream, error)
	read    func(closed <-chan struct{}) (host.StreamChunk, error)
	onClose func()

	lmu     sync.Mutex
	streams map[string]chan struct{}
	next    int
	// ends are the end_reason of the ignition stream log records.
	ends []string
}

func (h *ledgerHost) Log(_, _ string, fields map[string]any) {
	if fields["event"] != "ignition_stream" {
		return
	}
	reason, _ := fields["end_reason"].(string)
	h.lmu.Lock()
	h.ends = append(h.ends, reason)
	h.lmu.Unlock()
}

// endReasons are the end_reason of the ignition streams so far.
func (h *ledgerHost) endReasons() []string {
	h.lmu.Lock()
	defer h.lmu.Unlock()
	return append([]string(nil), h.ends...)
}

func (h *ledgerHost) OpenModelStream(ctx context.Context, req host.ModelRequest) (host.ModelStream, error) {
	h.fakeHost.mu.Lock()
	h.executed = append(h.executed, req)
	h.fakeHost.mu.Unlock()
	opened, err := h.open(ctx)
	if err != nil || opened.StatusCode < 200 || opened.StatusCode >= 300 {
		opened.ID = ""
		return opened, err
	}
	h.lmu.Lock()
	defer h.lmu.Unlock()
	if h.streams == nil {
		h.streams = map[string]chan struct{}{}
	}
	h.next++
	opened.ID = "s" + strconv.Itoa(h.next)
	h.streams[opened.ID] = make(chan struct{})
	return opened, nil
}

func (h *ledgerHost) ReadModelStream(id string) (host.StreamChunk, error) {
	h.lmu.Lock()
	closed := h.streams[id]
	h.lmu.Unlock()
	if closed == nil {
		return host.StreamChunk{}, errors.New("stream is not open")
	}
	return h.read(closed)
}

func (h *ledgerHost) CloseModelStream(id string) error {
	h.lmu.Lock()
	closed := h.streams[id]
	delete(h.streams, id)
	h.lmu.Unlock()
	if closed != nil {
		close(closed)
		if h.onClose != nil {
			h.onClose()
		}
	}
	return nil
}

// openStreams is the number of streams opened and not closed.
func (h *ledgerHost) openStreams() int {
	h.lmu.Lock()
	defer h.lmu.Unlock()
	return len(h.streams)
}

var (
	messageStop = host.StreamChunk{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), Done: true}
	// claudeUnauthorized is the gateway's answer to a rejected Claude token.
	claudeUnauthorized = &host.Error{Method: "POST /v1/messages", Code: "authentication_error", Status: 401,
		Message: `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`}
)

// blockUntilClosed is a read of a stream that sends nothing more.
func blockUntilClosed(closed <-chan struct{}) (host.StreamChunk, error) {
	<-closed
	return host.StreamChunk{}, errors.New("stream closed")
}

// newLedgerEngine returns an engine with a due Claude target whose
// requests go to a ledgerHost. queried counts the Claude quota queries,
// and fails the test when one runs while a stream is still open.
func newLedgerEngine(t *testing.T) (*Engine, *ledgerHost, target, *int) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	base := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, base, c)
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(context.Background(), cfg, c.t, "", false)
	h := &ledgerHost{fakeHost: base}
	queried := new(int)
	query := base.http
	base.http = func(req httpRequest) httpResponse {
		if req.URL == providers.ClaudeUsageURL {
			*queried++
			if n := h.openStreams(); n != 0 {
				t.Errorf("confirmation queried with %d streams open", n)
			}
		}
		return query(req)
	}
	e.host = h
	return e, h, claudeTarget(t, e), queried
}

// waitGoroutines waits until the goroutines of an ignition have ended.
func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines left: %d > %d\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Every way an ignition stream ends closes it before confirmation, and
// leaves no goroutine behind. The stream ends with its own reason, and
// only a stream without an execution error goes on to the confirmation
// queries.
func TestIgnitionClosesEveryStream(t *testing.T) {
	old, oldTimeout := confirmDelays, ignitionStreamTimeout
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays, ignitionStreamTimeout = old, oldTimeout }()
	ok := func(context.Context) (host.ModelStream, error) { return host.ModelStream{StatusCode: 200}, nil }
	notConfirmed := func(err error) bool { return errors.Is(err, ignite.ErrNotConfirmed) }
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		setup   func(e *Engine, h *ledgerHost)
		want    func(err error) bool
		reason  string
		queried bool
	}{
		{"completed, then confirmation queries", 0, func(e *Engine, h *ledgerHost) {
			h.open = ok
			h.read = func(<-chan struct{}) (host.StreamChunk, error) { return messageStop, nil }
		}, notConfirmed, "completed", true},
		{"read error", 0, func(e *Engine, h *ledgerHost) {
			h.open = ok
			h.read = func(<-chan struct{}) (host.StreamChunk, error) { return host.StreamChunk{}, errors.New("reset") }
		}, func(err error) bool { return err != nil && !notConfirmed(err) }, "read_error", false},
		{"output window", 0, func(e *Engine, h *ledgerHost) {
			h.open = ok
			first := true
			h.read = func(closed <-chan struct{}) (host.StreamChunk, error) {
				if first {
					first = false
					return host.StreamChunk{Payload: []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")}, nil
				}
				return blockUntilClosed(closed)
			}
		}, notConfirmed, "output_timeout", true},
		{"two-minute limit", 50 * time.Millisecond, func(e *Engine, h *ledgerHost) {
			h.open = ok
			h.read = blockUntilClosed
		}, func(err error) bool { return errors.Is(err, errIgnitionTimeout) }, "timeout", false},
		{"exit while reading", 0, func(e *Engine, h *ledgerHost) {
			h.open = ok
			h.read = func(closed <-chan struct{}) (host.StreamChunk, error) {
				go e.Cancel()
				return blockUntilClosed(closed)
			}
		}, func(err error) bool { return errors.Is(err, context.Canceled) }, "canceled", false},
		{"withdrawn after the headers", 0, func(e *Engine, h *ledgerHost) {
			h.open = func(context.Context) (host.ModelStream, error) {
				e.Configure(ignitionOff(e), nil)
				return host.ModelStream{StatusCode: 200}, nil
			}
			h.read = func(<-chan struct{}) (host.StreamChunk, error) { return messageStop, nil }
		}, notConfirmed, "completed", true},
		{"withdrawn before the headers", 0, func(e *Engine, h *ledgerHost) {
			h.open = func(ctx context.Context) (host.ModelStream, error) {
				e.Configure(ignitionOff(e), nil)
				<-ctx.Done()
				return host.ModelStream{}, ctx.Err()
			}
			h.read = blockUntilClosed
		}, func(err error) bool { return errors.Is(err, errIgnitionWithdrawn) }, "withdrawn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ignitionStreamTimeout = oldTimeout
			if tc.timeout > 0 {
				ignitionStreamTimeout = tc.timeout
			}
			e, h, chosen, queried := newLedgerEngine(t)
			tc.setup(e, h)
			before := runtime.NumGoroutine()
			err := e.igniteTarget(context.Background(), e.config(), chosen, false)
			if !tc.want(err) {
				t.Fatalf("err = %v", err)
			}
			if n := h.openStreams(); n != 0 || len(h.executed) != 1 {
				t.Fatalf("%d streams left open after %d requests", n, len(h.executed))
			}
			if ends := h.endReasons(); len(ends) != 1 || ends[0] != tc.reason {
				t.Fatalf("end reasons %v, want [%s]", ends, tc.reason)
			}
			if (*queried > 0) != tc.queried {
				t.Fatalf("%d confirmation queries, want some: %v", *queried, tc.queried)
			}
			waitGoroutines(t, before)
		})
	}
}

// Stopping the engine while an automatic ignition waits for its headers,
// reads its stream or confirms the window is no ignition failure: no
// retry, circuit or event, so a restart ignites as planned.
func TestExitDoesNotCountAsIgnitionFailure(t *testing.T) {
	old := confirmDelays
	defer func() { confirmDelays = old }()
	for _, phase := range []string{"model list", "headers", "stream", "confirmation", "confirmation query"} {
		t.Run(phase, func(t *testing.T) {
			confirmDelays = []time.Duration{time.Millisecond}
			e, h, chosen, _ := newLedgerEngine(t)
			switch phase {
			case "model list":
				// The list is not cached and no model stands in for it:
				// the stop cuts its request, and the gateway's error does
				// not say so.
				h.listModels = func() ([]string, error) {
					e.Cancel()
					return nil, &host.Error{Method: "GET /v1/models", Code: "gateway_unreachable", Message: "Get \"http://127.0.0.1:3425/v1/models\": context canceled"}
				}
			case "confirmation query":
				// The stop cuts the last confirmation query short.
				h.open = func(context.Context) (host.ModelStream, error) { return host.ModelStream{StatusCode: 200}, nil }
				h.read = func(<-chan struct{}) (host.StreamChunk, error) { return messageStop, nil }
				var sent atomic.Bool
				h.onClose = func() { sent.Store(true) }
				query := h.fakeHost.http
				h.fakeHost.http = func(req httpRequest) httpResponse {
					if sent.Load() && req.URL == providers.ClaudeUsageURL {
						e.Cancel()
						return httpResponse{StatusCode: http.StatusBadGateway}
					}
					return query(req)
				}
			case "headers":
				h.open = func(ctx context.Context) (host.ModelStream, error) {
					go e.Cancel()
					<-ctx.Done()
					return host.ModelStream{}, ctx.Err()
				}
			case "stream":
				h.open = func(context.Context) (host.ModelStream, error) { return host.ModelStream{StatusCode: 200}, nil }
				h.read = func(closed <-chan struct{}) (host.StreamChunk, error) {
					go e.Cancel()
					return blockUntilClosed(closed)
				}
			case "confirmation":
				confirmDelays = []time.Duration{5 * time.Second}
				h.open = func(context.Context) (host.ModelStream, error) { return host.ModelStream{StatusCode: 200}, nil }
				h.read = func(<-chan struct{}) (host.StreamChunk, error) { return messageStop, nil }
				h.onClose = func() {
					go func() {
						time.Sleep(20 * time.Millisecond)
						e.Cancel()
					}()
				}
			}
			err := e.igniteTarget(context.Background(), e.config(), chosen, false)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
			ts := e.state.Target(chosen.group.Key)
			if ts.ConsecutiveFailures != 0 || ts.LastError != "" || ts.RetryAtEpoch != 0 || ts.CircuitOpenUntilEpoch != 0 {
				t.Fatalf("an exit was recorded as a failure: %+v", ts)
			}
			if n := len(eventsOf(e, "ignite_failed")) + len(eventsOf(e, "ignite_paused")); n != 0 {
				t.Fatalf("%d failure events", n)
			}
		})
	}
}

// An error the provider answered before the stop is handled as that error.
func TestExitKeepsAnAnsweredRejection(t *testing.T) {
	e, h, chosen, _ := newLedgerEngine(t)
	h.open = func(context.Context) (host.ModelStream, error) {
		e.Cancel()
		return host.ModelStream{StatusCode: 401}, claudeUnauthorized
	}
	err := e.igniteTarget(context.Background(), e.config(), chosen, false)
	if host.StatusOf(err) != 401 || e.credentialProblem("3") != failure.ReasonAuth {
		t.Fatalf("err = %v, credential %+v", err, e.state.Credentials["3"])
	}
}

// The provider rejected the request: its error headers arrived, and the
// settings changed while the gateway read the error body. The request is
// no longer withdrawn; the rejection counts as a credential problem.
func TestRejectionReadDuringConfigChangeIsNotWithdrawn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    *host.Error
		reason string
	}{
		{"401", claudeUnauthorized, failure.ReasonAuth},
		{"403", &host.Error{Method: "POST /v1/messages", Code: "permission_error", Status: 403, Message: tosError.Message}, failure.ReasonSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, h, chosen, _ := newLedgerEngine(t)
			var cause error
			h.open = func(ctx context.Context) (host.ModelStream, error) {
				// The headers are in; the body follows after the change.
				e.Configure(ignitionOff(e), nil)
				cause = context.Cause(ctx)
				return host.ModelStream{StatusCode: tc.err.Status, Header: http.Header{}}, tc.err
			}
			err := e.igniteTarget(context.Background(), e.config(), chosen, false)
			if errors.Is(err, errIgnitionWithdrawn) || host.StatusOf(err) != tc.err.Status {
				t.Fatalf("err = %v", err)
			}
			if !errors.Is(cause, errIgnitionWithdrawn) {
				t.Fatalf("the test needs the withdrawal during the body: %v", cause)
			}
			if got := e.credentialProblem("3"); got != tc.reason {
				t.Fatalf("credential problem %q, want %q", got, tc.reason)
			}
			if paused := eventsOf(e, "ignite_paused"); len(paused) != 1 || paused[0].Params["reason"] != tc.reason {
				t.Fatalf("paused events %+v", paused)
			}
		})
	}
}
