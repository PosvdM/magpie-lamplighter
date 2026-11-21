package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/redact"
)

const ignitionOutputWindow = 2 * time.Second

// ignitionStreamTimeout bounds one ignition request from sending it to the
// end of its body. The gateway sends keepalives while it waits for a slow
// start, such as a Claude Code run, so a stream can otherwise stay open
// without output and hold up the background loop.
var ignitionStreamTimeout = 2 * time.Minute

// errIgnitionTimeout ends a stream that reached ignitionStreamTimeout.
var errIgnitionTimeout = errors.New("点火请求超时")

// streamEnd gives the reader and watchdog a single terminal owner. Closing done
// publishes the result; a late deadline, cancellation or panic cannot replace it.
type streamEnd struct {
	mu     sync.Mutex
	done   chan struct{}
	reason string
	err    error
}

func (s *streamEnd) claim(reason string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" {
		return false
	}
	s.reason, s.err = reason, err
	close(s.done)
	return true
}

func (s *streamEnd) result() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, s.err
}

type streamTimer struct {
	deadline <-chan time.Time
	stop     func()
}

func newStreamTimer(wait time.Duration) streamTimer {
	timer := time.NewTimer(wait)
	return streamTimer{deadline: timer.C, stop: func() { timer.Stop() }}
}

// watchStream only closes an active stream after claiming its terminal result.
// The timer factory allows tests to make completion and deadline ready together.
func watchStream(ctx context.Context, stop <-chan struct{}, first <-chan time.Time, window time.Duration, end *streamEnd, closeStream func(), newTimer func(time.Duration) streamTimer) {
	cutoff := func(reason string, err error) {
		if end.claim(reason, err) {
			closeStream()
		}
	}
	defer func() {
		if recover() != nil {
			cutoff("watchdog_error", errors.New("ignition stream watchdog failed"))
		}
	}()
	var timer streamTimer
	defer func() {
		if timer.stop != nil {
			timer.stop()
		}
	}()
	for {
		select {
		case <-end.done:
			return
		case at := <-first:
			timer = newTimer(time.Until(at.Add(window)))
		case <-timer.deadline:
			// A cancellation already visible at cutoff must not be reported as
			// deliberate success; cancellation after a terminal claim is late.
			select {
			case <-ctx.Done():
				cutoff("canceled", context.Canceled)
			case <-stop:
				cutoff("canceled", context.Canceled)
			default:
				cutoff("output_timeout", nil)
			}
			return
		case <-ctx.Done():
			cutoff("canceled", context.Canceled)
			return
		case <-stop:
			cutoff("canceled", context.Canceled)
			return
		}
	}
}

// sentResponse is an ignition response with sentAt, when its request
// entered OpenModelStream; confirmation compares readings with it.
type sentResponse struct {
	host.ModelResponse
	sentAt time.Time
	// confirmed tells that the account of an agy-cli request
	// (identityChecked) was confirmed right before it was sent, with the
	// sign-in fingerprint.
	confirmed   bool
	fingerprint string
}

// executeStream retains headers for quota confirmation. A deliberate cutoff
// returns no execution error, so it cannot cause a model fallback. It does not
// establish ignition success: the caller must still confirm the quota window.
//
// The request of an automatic ignition (t.automatic) passes admit right
// before it is opened, and returns errIgnitionWithdrawn when the target no
// longer qualifies or a configuration change withdraws it before its
// response headers arrive.
func (e *Engine) executeStream(ctx context.Context, t target, req host.ModelRequest, outputWindow time.Duration) (resp sentResponse, err error) {
	started := time.Now()
	correlation := rand.Text()
	var firstOutput time.Time
	var capture streamCapture
	var streamID string
	reason := "start_error"
	defer func() {
		usageState := "missing"
		if len(capture.usage) > 0 {
			usageState = "partial"
			if capture.completeUsage(t.group.Provider, req.Protocol) && capture.terminal && reason == "completed" {
				usageState = "complete"
			}
		}
		var firstWait any
		if !firstOutput.IsZero() {
			firstWait = firstOutput.Sub(started).Milliseconds()
		}
		// The reply is the record's only free text; it is redacted
		// (redact.Text), as every log line is.
		fields := map[string]any{
			"event": "ignition_stream", "provider": t.group.Provider,
			"model": req.Model, "auth_index": t.group.AuthIndex,
			"correlation_id": correlation, "stream_id": streamID,
			"reply": redact.Text(capture.reply.String()), "reply_truncated": capture.replyChars > replyLimit,
			"output_received": capture.output, "usage": capture.usage, "usage_source": "magpie_gateway",
			"usage_completeness": usageState, "end_reason": reason,
			"first_output_wait_ms": firstWait, "total_ms": time.Since(started).Milliseconds(),
			"status_code": resp.StatusCode,
		}
		// JSON in Message keeps the record whole in a log that drops
		// fields, and escapes newlines and control characters.
		raw, _ := json.Marshal(fields)
		level := "info"
		if err != nil {
			level = "warn"
		}
		defer func() { _ = recover() }()
		e.host.Log(level, "Lamplighter ignition "+string(raw), fields)
	}()
	if ctx.Err() != nil || e.stopping() {
		reason = "canceled"
		return resp, context.Canceled
	}

	// streamCtx carries the request: cancelling ctx, stopping or reaching
	// ignitionStreamTimeout cancels it, also while it waits for headers.
	streamCtx, cancelStream := context.WithTimeout(ctx, ignitionStreamTimeout)
	defer cancelStream()
	go func() {
		select {
		case <-e.stopCh:
			cancelStream()
		case <-streamCtx.Done():
		}
	}()
	timedOut := func() bool {
		return ctx.Err() == nil && errors.Is(streamCtx.Err(), context.DeadlineExceeded)
	}
	// reqCtx carries the request; withdrawing an admitted request cancels
	// it. It ends with streamCtx.
	reqCtx := streamCtx
	var admitted func() bool
	if t.automatic {
		var ok bool
		reqCtx, admitted, ok = e.admit(streamCtx, t)
		if !ok && e.stopping() {
			reason = "canceled"
			return resp, context.Canceled
		}
		if !ok {
			reason = "withdrawn"
			return resp, errIgnitionWithdrawn
		}
		// Ends the admission also when OpenModelStream panics.
		defer admitted()
	}
	resp.sentAt = e.now()
	opened, err := e.host.OpenModelStream(reqCtx, req)
	// A response status means the headers arrived: the request reached the
	// provider, and its outcome counts even when a withdrawal or a stop came
	// at the same moment. The host no longer ends the request on reqCtx.
	answered := opened.StatusCode != 0 || host.StatusOf(err) != 0
	if admitted != nil && !admitted() && !answered {
		if err == nil && opened.ID != "" {
			_ = e.host.CloseModelStream(opened.ID)
		}
		reason = "withdrawn"
		return resp, errIgnitionWithdrawn
	}
	resp.StatusCode, resp.Header = opened.StatusCode, opened.Header
	streamID = opened.ID
	if err != nil {
		switch {
		case answered:
			reason = "http_error"
		case ctx.Err() != nil || e.stopping():
			reason = "canceled"
			return resp, context.Canceled
		case timedOut():
			reason = "timeout"
			return resp, errIgnitionTimeout
		}
		return resp, err
	}
	if streamID == "" {
		return resp, errors.New("ignition stream ID missing")
	}

	var closeOnce sync.Once
	var closeErr error
	closeStream := func() {
		closeOnce.Do(func() {
			defer func() {
				if recover() != nil {
					closeErr = errors.New("ignition stream close panic")
				}
			}()
			closeErr = e.host.CloseModelStream(streamID)
		})
	}
	first := make(chan time.Time, 1)
	end := &streamEnd{done: make(chan struct{})}
	finish := func(why string, failure error) (sentResponse, error) {
		end.claim(why, failure)
		return resp, failure
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watchStream(ctx, e.stopCh, first, outputWindow, end, closeStream, newStreamTimer)
	}()
	// The deadline closes the stream for any host, also one whose read does
	// not follow streamCtx.
	stopDeadline := context.AfterFunc(streamCtx, func() {
		if timedOut() && end.claim("timeout", errIgnitionTimeout) {
			closeStream()
		}
	})
	defer stopDeadline()
	defer func() {
		// Also release the watchdog if the reader unwinds through a panic.
		end.claim("read_error", errors.New("ignition stream reader failed"))
		<-watchDone
		closeStream()
		reason, err = end.result()
		if closeErr != nil {
			// Cleanup failure is never deliberate success. Preserve the owned
			// execution error as well, including context.Canceled for callers.
			reason, err = "close_error", errors.Join(err, errors.New("ignition stream close failed"))
		}
	}()
	capture.onOutput = func() {
		firstOutput = time.Now()
		first <- firstOutput
	}
	if opened.StatusCode < 200 || opened.StatusCode >= 300 {
		return finish("http_error", &host.Error{Method: "gateway", Code: "model_execution_failed", Message: "ignition model request failed", Status: opened.StatusCode})
	}
	for {
		if ctx.Err() != nil || e.stopping() {
			return finish("canceled", context.Canceled)
		}
		chunk, readErr := e.host.ReadModelStream(streamID)
		if readErr != nil {
			if timedOut() {
				return finish("timeout", errIgnitionTimeout)
			}
			if ctx.Err() != nil || e.stopping() {
				return finish("canceled", context.Canceled)
			}
			return finish("read_error", errors.New("ignition stream read failed"))
		}
		if parseErr := capture.feedChunk(req.Protocol, chunk.Payload); parseErr != nil {
			return finish("stream_error", parseErr)
		}
		if chunk.Error != "" {
			return finish("stream_error", streamErrorText(chunk.Error))
		}
		if chunk.Done {
			if parseErr := capture.finish(); parseErr != nil {
				return finish("stream_error", parseErr)
			}
		}
		if capture.terminal {
			return finish("completed", nil)
		}
		if chunk.Done {
			return finish("unexpected_eof", errors.New("ignition stream ended without terminal event"))
		}
	}
}
