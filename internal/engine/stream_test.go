package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/ignite"
)

// streamHost blocks reads until a chunk arrives or Close cancels the upstream.
// This models the host bridge rather than timing out only the caller's wait.
type streamHost struct {
	*fakeHost
	chunks    chan host.StreamChunk
	closed    chan struct{}
	reading   chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
	calls     int
	opened    host.ModelStream
	openErr   error
	open      func()
	messages  []string
}

func newStreamHost() *streamHost {
	return &streamHost{
		fakeHost: &fakeHost{}, chunks: make(chan host.StreamChunk, 10),
		closed: make(chan struct{}), reading: make(chan struct{}, 10),
		opened: host.ModelStream{StatusCode: 200, ID: "stream-1", Header: http.Header{"X-Test": {"quota"}}},
	}
}

func (h *streamHost) OpenModelStream(_ context.Context, req host.ModelRequest) (host.ModelStream, error) {
	h.calls++
	if h.open != nil {
		h.open()
	}
	return h.opened, h.openErr
}

func (h *streamHost) ReadModelStream(string) (host.StreamChunk, error) {
	select {
	case h.reading <- struct{}{}:
	default:
	}
	select {
	case chunk := <-h.chunks:
		return chunk, nil
	case <-h.closed:
		return host.StreamChunk{}, errors.New("upstream canceled SECRET")
	}
}

func (h *streamHost) CloseModelStream(string) error {
	h.mu.Lock()
	h.closes++
	h.mu.Unlock()
	h.closeOnce.Do(func() { close(h.closed) })
	return nil
}

func (h *streamHost) Log(_ string, message string, _ map[string]any) {
	h.messages = append(h.messages, message)
}

func (h *streamHost) raw(s string) {
	h.chunks <- host.StreamChunk{Payload: []byte(s)}
}

func (h *streamHost) event(s string) {
	h.raw("data: " + s + "\n\n")
}

func streamTarget() target {
	return target{group: GroupView{Provider: "codex", AuthIndex: "42"}}
}

func streamRequest() host.ModelRequest {
	return host.ModelRequest{Model: "test-model", Protocol: "openai-response", Body: []byte("SECRET request"), Account: "SECRET credential"}
}

func streamRecord(t *testing.T, h *streamHost) map[string]any {
	t.Helper()
	if len(h.messages) != 1 {
		t.Fatalf("want one per-call log, got %v", h.messages)
	}
	message := h.messages[0]
	if strings.Contains(message, "SECRET") || strings.Contains(message, "\n") {
		t.Fatalf("unsafe log message: %s", message)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "Lamplighter ignition ")), &record); err != nil {
		t.Fatal(err)
	}
	if record["provider"] != "codex" || record["model"] != "test-model" || record["auth_index"] != "42" || record["correlation_id"] == "" {
		t.Fatal(record)
	}
	return record
}

func TestIgnitionStreamWatchdogClosesBlockedRead(t *testing.T) {
	h := newStreamHost()
	h.raw("event: response.output_text.delta\n")
	h.event(`{"type":"response.output_text.delta","delta":"OK"}`)
	e := New(Options{Host: h})
	started := time.Now()
	resp, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
	elapsed := time.Since(started)
	if err != nil || elapsed < 1900*time.Millisecond || elapsed > 5*time.Second || h.closes != 1 {
		t.Fatalf("err=%v elapsed=%s closes=%d", err, elapsed, h.closes)
	}
	if resp.Header.Get("X-Test") != "quota" {
		t.Fatal("lost headers for quota confirmation")
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "output_timeout" || r["reply"] != "OK" || r["usage_completeness"] != "missing" || r["first_output_wait_ms"] == nil {
		t.Fatal(r)
	}
}

func TestIgnitionStreamNaturalEndAndLog(t *testing.T) {
	h := newStreamHost()
	h.event(`{"type":"response.output_item.added","item":{"encrypted_content":"SECRET"}}`)
	h.event(`{"type":"response.reasoning_summary_text.delta","delta":"brief\n"}`)
	h.event(`{"type":"response.function_call_arguments.delta","delta":"{}"}`)
	text, _ := json.Marshal(strings.Repeat("好🙂", 2500))
	h.event(`{"type":"response.output_text.delta","delta":` + string(text) + `}`)
	h.event(`{"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":19,"output_tokens_details":{"reasoning_tokens":7,"encrypted_content":"SECRET"}}}}`)
	e := New(Options{Host: h})
	started := time.Now()
	_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
	if err != nil || time.Since(started) > time.Second || h.closes != 1 {
		t.Fatalf("err=%v closes=%d", err, h.closes)
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "completed" || r["usage_completeness"] != "complete" || r["reply_truncated"] != true || len([]rune(str(r["reply"]))) != replyLimit || !strings.HasPrefix(str(r["reply"]), "brief\n{}") {
		t.Fatal(r)
	}
	if object(r["usage"])["output_tokens"] != float64(19) {
		t.Fatal("usage must be reported, not estimated")
	}
}

// The gateway sends Responses events as standard SSE with event lines and
// keepalive comments, split anywhere.
func TestIgnitionResponsesSSE(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(strconv.FormatBool(split), func(t *testing.T) {
			h := newStreamHost()
			wire := ": keepalive\n\n" +
				"event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n"
			// More chunks than the channel holds: feed them as they are read.
			go func() {
				rest := wire
				if split {
					for len(rest) > 7 {
						h.raw(rest[:7])
						rest = rest[7:]
					}
				}
				h.raw(rest)
				h.chunks <- host.StreamChunk{Done: true}
			}()
			e := New(Options{Host: h})
			_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
			if err != nil || h.closes != 1 {
				t.Fatalf("err=%v closes=%d", err, h.closes)
			}
			r := streamRecord(t, h)
			if r["end_reason"] != "completed" || r["reply"] != "OK" || r["usage_completeness"] != "complete" || object(r["usage"])["output_tokens"] != float64(3) {
				t.Fatal(r)
			}
		})
	}
}

func TestIgnitionResponsesFailures(t *testing.T) {
	for _, typ := range []string{"response.failed", "response.incomplete", "response.created", "response.output_text.delta"} {
		t.Run(typ, func(t *testing.T) {
			h := newStreamHost()
			h.raw(": keepalive\n\n")
			h.raw("event: " + typ + "\n")
			h.event(`{"type":"` + typ + `","delta":"OK"}`)
			h.chunks <- host.StreamChunk{Done: true}
			e := New(Options{Host: h})
			_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
			if err == nil || h.closes != 1 {
				t.Fatalf("err=%v closes=%d", err, h.closes)
			}
			r := streamRecord(t, h)
			if r["end_reason"] == "completed" || r["usage_completeness"] != "missing" {
				t.Fatal(r)
			}
			if (typ == "response.created" || typ == "response.output_text.delta") && !strings.Contains(err.Error(), "without terminal event") {
				t.Fatal(err)
			}
		})
	}
}

// The gateway's Chat stream is SSE ending in data: [DONE]; a stream that
// ends without it still ends at EOF after its finish_reason.
func TestIgnitionChatSSE(t *testing.T) {
	for _, mode := range []string{"done", "eof", "cutoff"} {
		h := newStreamHost()
		h.event(`{"choices":[{"delta":{"content":"OK"},"finish_reason":null}]}`)
		if mode != "cutoff" {
			h.event(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			h.event(`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
			if mode == "done" {
				h.event(`[DONE]`)
			} else {
				h.chunks <- host.StreamChunk{Done: true}
			}
		}
		e := New(Options{Host: h})
		req := streamRequest()
		req.Protocol = "openai"
		_, err := e.executeStream(context.Background(), streamTarget(), req, 20*time.Millisecond)
		if err != nil || h.closes != 1 {
			t.Fatalf("%s: err=%v closes=%d", mode, err, h.closes)
		}
		r := streamRecord(t, h)
		if r["reply"] != "OK" || r["first_output_wait_ms"] == nil {
			t.Fatal(r)
		}
		if mode == "cutoff" {
			if r["end_reason"] != "output_timeout" || r["usage_completeness"] != "missing" {
				t.Fatal(r)
			}
		} else if r["end_reason"] != "completed" || r["usage_completeness"] != "partial" {
			t.Fatal(mode, r)
		}
	}
}

func TestIgnitionStreamUsageProvenance(t *testing.T) {
	for _, tc := range []struct {
		provider, protocol, completeness string
		chunks                           []string
	}{
		{"antigravity", "openai", "partial", []string{
			`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":0}}`,
		}},
		{"antigravity", "openai", "partial", []string{
			`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}`,
		}},
		{"claude", "claude", "complete", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}`,
			`{"type":"message_delta","usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		}},
		{"codex", "openai-response", "complete", []string{
			`{"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":9}}}`,
		}},
	} {
		t.Run(tc.provider+"/"+tc.chunks[0], func(t *testing.T) {
			h := newStreamHost()
			for _, chunk := range tc.chunks {
				h.event(chunk)
			}
			h.chunks <- host.StreamChunk{Done: true}
			e := New(Options{Host: h})
			target, req := streamTarget(), streamRequest()
			target.group.Provider, req.Protocol = tc.provider, tc.protocol
			_, err := e.executeStream(context.Background(), target, req, ignitionOutputWindow)
			if err != nil || h.closes != 1 || len(h.messages) != 1 {
				t.Fatalf("err=%v closes=%d logs=%v", err, h.closes, h.messages)
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(h.messages[0], "Lamplighter ignition ")), &record); err != nil {
				t.Fatal(err)
			}
			if record["usage_completeness"] != tc.completeness || record["usage_source"] != "magpie_gateway" || record["end_reason"] != "completed" {
				t.Fatal(record)
			}
		})
	}
}

func TestIgnitionStreamLifecycleDoesNotStartTimer(t *testing.T) {
	h := newStreamHost()
	h.event(`{"type":"response.created"}`)
	h.event(`{"choices":[{"delta":{"role":"assistant","content":""}}]}`)
	h.event(`{"type":"ping"}`)
	e := New(Options{Host: h})
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	_, err := e.executeStream(ctx, streamTarget(), streamRequest(), 10*time.Millisecond)
	if !errors.Is(err, context.Canceled) || h.closes != 1 {
		t.Fatalf("err=%v closes=%d", err, h.closes)
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "canceled" || r["first_output_wait_ms"] != nil || r["output_received"] != false {
		t.Fatal(r)
	}
}

func TestIgnitionStreamStopAndStartupCancellation(t *testing.T) {
	for _, phase := range []string{"before call", "during startup", "blocked read"} {
		t.Run(phase, func(t *testing.T) {
			h := newStreamHost()
			e := New(Options{Host: h})
			var stopperDone chan struct{}
			switch phase {
			case "before call":
				close(e.stopCh)
			case "during startup":
				// Stopping while the request starts cancels it.
				h.open = func() { close(e.stopCh) }
			case "blocked read":
				stopperDone = make(chan struct{})
				go func() {
					defer close(stopperDone)
					<-h.reading
					close(e.stopCh)
				}()
			}
			_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
			if stopperDone != nil {
				<-stopperDone
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if phase == "before call" {
				if h.calls != 0 || h.closes != 0 {
					t.Fatal("started request after stop")
				}
			} else if h.closes != 1 {
				t.Fatalf("leaked stream: closes=%d", h.closes)
			}
			if streamRecord(t, h)["end_reason"] != "canceled" {
				t.Fatal(h.messages)
			}
		})
	}
}

func TestIgnitionStreamErrorsAndPartialUsage(t *testing.T) {
	for _, mode := range []string{"startup", "http", "error", "read terminal error", "eof", "invalid", "partial cutoff"} {
		t.Run(mode, func(t *testing.T) {
			h := newStreamHost()
			switch mode {
			case "startup":
				h.opened = host.ModelStream{}
				h.openErr = &host.Error{Code: "gateway_http_error", Message: "SECRET", Status: 503}
			case "http":
				h.opened.StatusCode = 400
			case "error":
				h.event(`{"error":{"message":"SECRET"}}`)
			case "read terminal error":
				h.chunks <- host.StreamChunk{Error: "SECRET", Done: true}
			case "eof":
				h.chunks <- host.StreamChunk{Done: true}
			case "invalid":
				h.event(`SECRET invalid json`)
			case "partial cutoff":
				h.event(`{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}`)
				h.event(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"visible"}}`)
			}
			e := New(Options{Host: h})
			_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), 10*time.Millisecond)
			if (err == nil) != (mode == "partial cutoff") {
				t.Fatal(err)
			}
			r := streamRecord(t, h)
			if mode == "partial cutoff" && (r["usage_completeness"] != "partial" || r["reply"] != "visible") {
				t.Fatal(r)
			}
		})
	}
}

// Hold the watchdog inside timer creation until the reader owns a result.
// Both done and deadline are then ready at the same select, without sleeps.
func TestStreamEndReaderBeatsReadyDeadline(t *testing.T) {
	parseErr := (&streamCapture{}).event([]byte(`{"error":{"message":"SECRET"}}`))
	readErr := errors.New("ignition stream read failed")
	for _, tc := range []struct {
		reason string
		err    error
	}{
		{"completed", nil}, {"stream_error", parseErr}, {"read_error", readErr},
		{"unexpected_eof", errors.New("ignition stream ended without terminal event")},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			end := &streamEnd{done: make(chan struct{})}
			first := make(chan time.Time, 1)
			first <- time.Now()
			deadline := make(chan time.Time, 1)
			creating, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			closes, stops := 0, 0
			go func() {
				defer close(joined)
				watchStream(context.Background(), nil, first, ignitionOutputWindow, end, func() { closes++ }, func(time.Duration) streamTimer {
					close(creating)
					<-release
					return streamTimer{deadline: deadline, stop: func() { stops++ }}
				})
			}()
			<-creating
			if !end.claim(tc.reason, tc.err) {
				t.Fatal("reader could not claim active stream")
			}
			deadline <- time.Now()
			close(release)
			<-joined
			// Exercise the cutoff arbitration directly as well, independent of
			// which of the two ready select cases the runtime chooses.
			if end.claim("output_timeout", nil) {
				t.Fatal("late deadline replaced the reader")
			}
			reason, err := end.result()
			if reason != tc.reason || err != tc.err || closes != 0 || stops != 1 {
				t.Fatalf("reason=%s err=%v closes=%d stops=%d", reason, err, closes, stops)
			}
		})
	}
}

func TestStreamWatchdogOwnershipAndPanic(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "stop", "panic", "late panic", "late cancel", "late stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := make(chan struct{})
			first := make(chan time.Time, 1)
			first <- time.Now()
			deadline := make(chan time.Time, 1)
			end := &streamEnd{done: make(chan struct{})}
			want := "output_timeout"
			closes := 0
			watchStream(ctx, stop, first, ignitionOutputWindow, end, func() { closes++ }, func(time.Duration) streamTimer {
				switch mode {
				case "cancel", "late cancel":
					cancel()
					want = "canceled"
				case "stop", "late stop":
					close(stop)
					want = "canceled"
				case "panic":
					panic("SECRET")
				}
				if strings.HasPrefix(mode, "late ") {
					end.claim("completed", nil)
					want = "completed"
					if mode == "late panic" {
						panic("SECRET")
					}
				}
				deadline <- time.Now()
				return streamTimer{deadline: deadline, stop: func() {}}
			})
			if mode == "panic" {
				want = "watchdog_error"
			}
			reason, err := end.result()
			wantCloses := 1
			if want == "completed" {
				wantCloses = 0
			}
			if reason != want || closes != wantCloses || ((err == nil) != (want == "completed" || want == "output_timeout")) {
				t.Fatalf("reason=%s err=%v closes=%d want=%s", reason, err, closes, want)
			}
			if end.claim("read_error", errors.New("closed reader")) {
				t.Fatal("read error replaced watchdog owner")
			}
		})
	}
}

type cleanupStreamHost struct {
	*streamHost
	read    func() (host.StreamChunk, error)
	cleanup func() error
}

func (h *cleanupStreamHost) ReadModelStream(id string) (host.StreamChunk, error) {
	if h.read != nil {
		return h.read()
	}
	return h.streamHost.ReadModelStream(id)
}

func (h *cleanupStreamHost) CloseModelStream(id string) error {
	_ = h.streamHost.CloseModelStream(id)
	return h.cleanup()
}

func TestIgnitionStreamCleanupPriority(t *testing.T) {
	for _, owner := range []string{"completed", "read_error", "canceled", "output_timeout"} {
		for _, cleanup := range []string{"ok", "error", "panic"} {
			t.Run(owner+"/"+cleanup, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h := &cleanupStreamHost{streamHost: newStreamHost()}
				switch owner {
				case "completed":
					h.event(`{"type":"response.completed"}`)
				case "output_timeout":
					h.event(`{"type":"response.output_text.delta","delta":"OK"}`)
				case "read_error", "canceled":
					h.read = func() (host.StreamChunk, error) {
						if owner == "canceled" {
							cancel()
							// Block until the watchdog closes upstream, ensuring
							// cancellation owns the terminal result first.
							<-h.closed
						}
						return host.StreamChunk{}, errors.New("SECRET")
					}
				}
				h.cleanup = func() error {
					cancel() // Cancellation during cleanup cannot replace an owner.
					switch cleanup {
					case "panic":
						panic("SECRET")
					case "error":
						return errors.New("SECRET")
					}
					return nil
				}
				e := New(Options{Host: h})
				_, err := e.executeStream(ctx, streamTarget(), streamRequest(), 0)
				wantReason := owner
				if cleanup != "ok" {
					wantReason = "close_error"
				}
				if streamRecord(t, h.streamHost)["end_reason"] != wantReason || h.closes != 1 {
					t.Fatalf("reason=%v closes=%d", h.messages, h.closes)
				}
				wantError := cleanup != "ok" || owner == "read_error" || owner == "canceled"
				if (err != nil) != wantError || (owner == "canceled" && !errors.Is(err, context.Canceled)) || (err != nil && strings.Contains(err.Error(), "SECRET")) {
					t.Fatalf("err=%v wantError=%v", err, wantError)
				}
			})
		}
	}
}

func TestIgnitionStreamErrorsKeepFailureProtection(t *testing.T) {
	for _, channel := range []string{"host", "error", "response.failed"} {
		for _, failure := range []string{"rate_limit_exceeded", "authentication_error", "model_not_found", "magpie_rate_limit", "explicit_authentication_status", "usage_limit_reached", "suspended"} {
			t.Run(channel+"/"+failure, func(t *testing.T) {
				c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
				base := claudeIgnitionHost(c, nil)
				e, _ := newTestEngine(t, base, c)
				e.poll(context.Background(), e.config(), c.t, "", false)
				h := newStreamHost()
				h.fakeHost = base
				h.opened.Header = http.Header{
					"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"},
					"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(5*time.Hour).Unix(), 10)},
				}
				h.open = func() {
					if h.calls > 1 {
						h.closed = make(chan struct{})
						h.closeOnce = sync.Once{}
						h.event(`{"type":"message_stop"}`)
						return
					}
					// The HTTP request has opened and returned a lifecycle event,
					// but this does not establish successful ignition.
					h.event(`{"type":"message_start"}`)
					detail := `{"code":"` + failure + `","type":"` + failure + `","message":"SECRET"}`
					switch failure {
					case "magpie_rate_limit":
						// Magpie's stream error after its keepalive 200: the
						// status only in the fixed suffix of the message.
						detail = `{"type":"api_error","message":"SECRET upstream said no (429: you can retry your request)"}`
					case "explicit_authentication_status":
						detail = `{"status":401,"message":"SECRET"}`
					case "usage_limit_reached":
						detail = `{"type":"usage_limit_reached","message":"SECRET"}`
					case "suspended":
						detail = `{"code":403,"message":"SECRET","status":"PERMISSION_DENIED","details":[{"reason":"TOS_VIOLATION","metadata":{"appeal_url":"https://forms.gle/hGzM9MEUv2azZsrb9"}}]}`
					}
					switch channel {
					case "host":
						h.chunks <- host.StreamChunk{Error: `{"error":` + detail + `}`, Done: true}
					case "error":
						h.event(`{"type":"error","error":` + detail + `}`)
					case "response.failed":
						h.event(`{"type":"response.failed","response":{"error":` + detail + `}}`)
					}
				}
				e.host = h
				var chosen target
				for _, candidate := range e.collectTargets(e.config()) {
					if candidate.group.Provider == "claude" {
						chosen = candidate
					}
				}
				if chosen.cred == nil {
					t.Fatal("missing Claude target")
				}
				err := e.igniteTarget(context.Background(), e.config(), chosen, false)
				ts := e.state.Target(chosen.group.Key)
				// A rejected credential and a disabled account wait for a
				// new credential instead of the next day.
				waits := strings.Contains(failure, "authentication") || failure == "suspended"
				if failure == "model_not_found" {
					if err != nil || h.calls != 2 || h.closes != 2 || ts.LastSuccessEpoch == 0 || len(e.refused) != 1 {
						t.Fatalf("model fallback: err=%v calls=%d closes=%d state=%+v refused=%v", err, h.calls, h.closes, ts, e.refused)
					}
				} else if paused := ts.CircuitOpenUntilEpoch != 0; err == nil || ignite.Classify(err) != ignite.Hard || h.calls != 1 || h.closes != 1 || ts.ConsecutiveFailures != 1 || ts.LastSuccessEpoch != 0 ||
					waits != (!paused && e.credentialBlocked(chosen.cred.AuthIndex)) || !waits && !paused {
					t.Fatalf("hard failure protection: err=%v calls=%d closes=%d state=%+v", err, h.calls, h.closes, ts)
				}
				if strings.Contains(ts.LastError+strings.Join(h.messages, ""), "SECRET") {
					t.Fatal("raw provider message escaped into state or logs")
				}
				if failure == "suspended" {
					st := e.state.Credentials[chosen.cred.AuthIndex]
					if st == nil || st.Reason != "suspended" || st.Code != "TOS_VIOLATION" || st.AppealURL != "https://forms.gle/hGzM9MEUv2azZsrb9" {
						t.Fatalf("suspension in the stream: %+v", st)
					}
				}
			})
		}
	}
}

func TestIgnitionCutoffConfirmsQuotaWindow(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, base, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	h := newStreamHost()
	h.fakeHost = base
	h.opened.Header = http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(5*time.Hour).Unix(), 10)},
	}
	h.event(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`)
	e.host = h
	var chosen target
	for _, candidate := range e.collectTargets(e.config()) {
		if candidate.group.Provider == "claude" {
			chosen = candidate
		}
	}
	if chosen.cred == nil {
		t.Fatal("missing Claude target")
	}
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); err != nil || h.calls != 1 || h.closes != 1 {
		t.Fatalf("err=%v calls=%d closes=%d", err, h.calls, h.closes)
	}
	ts := e.state.Target(chosen.group.Key)
	if ts.LastSuccessEpoch == 0 || ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 || !e.confirmed(chosen.group.Key, ignitionBase{start: c.t}, c.t) {
		t.Fatalf("quota window not confirmed: %+v", ts)
	}
	if ev := e.events[len(e.events)-1]; ev.Kind != "ignite_manual" || ev.Params["model"] == "" {
		t.Fatalf("manual success was not recorded: %+v", ev)
	}
	var record map[string]any
	for _, message := range h.messages {
		if strings.HasPrefix(message, "Lamplighter ignition ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "Lamplighter ignition ")), &record); err != nil {
				t.Fatal(err)
			}
		}
	}
	if record["end_reason"] != "output_timeout" || record["reply"] != "OK" {
		t.Fatal(record)
	}
}

func TestIgnitionCutoffDoesNotFallbackOrConfirmFromBody(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, base, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	h := newStreamHost()
	h.fakeHost = base
	h.event(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`)
	e.host = h
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	var chosen target
	for _, candidate := range e.collectTargets(e.config()) {
		if candidate.group.Provider == "claude" {
			chosen = candidate
		}
	}
	if chosen.cred == nil {
		t.Fatal("missing Claude target")
	}
	err := e.igniteTarget(context.Background(), e.config(), chosen, true)
	if !errors.Is(err, ignite.ErrNotConfirmed) || h.calls != 1 || h.closes != 1 {
		t.Fatalf("err=%v calls=%d closes=%d", err, h.calls, h.closes)
	}
	if e.state.Target(chosen.group.Key).LastSuccessEpoch != 0 {
		t.Fatal("body incorrectly established ignition success")
	}
}

// A stream that sends nothing but keepalives ends at ignitionStreamTimeout,
// and its request is cancelled.
func TestIgnitionStreamTimeout(t *testing.T) {
	old := ignitionStreamTimeout
	ignitionStreamTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ignitionStreamTimeout = old })
	h := newStreamHost()
	h.raw(": keepalive\n\n")
	e := New(Options{Host: h})
	_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
	if !errors.Is(err, errIgnitionTimeout) || h.closes != 1 {
		t.Fatalf("err=%v closes=%d", err, h.closes)
	}
	if ignite.Classify(err) != ignite.Transient {
		t.Fatal("a timeout must be retried, not paused")
	}
	if r := streamRecord(t, h); r["end_reason"] != "timeout" {
		t.Fatal(r)
	}
}

// The gateway's error status arrives with OpenModelStream's error.
func TestIgnitionStreamOpenHTTPError(t *testing.T) {
	h := newStreamHost()
	h.opened = host.ModelStream{StatusCode: 429}
	h.openErr = &host.Error{Code: "gateway_http_error", Message: "SECRET", Status: 429}
	e := New(Options{Host: h})
	_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
	if host.StatusOf(err) != 429 || h.closes != 0 {
		t.Fatalf("err=%v closes=%d", err, h.closes)
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "http_error" || r["status_code"] != float64(429) {
		t.Fatal(r)
	}
}
