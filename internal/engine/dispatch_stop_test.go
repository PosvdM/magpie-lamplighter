package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// newDispatchEngine returns an engine that reports running, with no loop:
// the test takes the commands from cmdCh itself, or leaves them waiting.
func newDispatchEngine(t *testing.T) *Engine {
	t.Helper()
	e := New(Options{Version: "test", DataDir: t.TempDir()})
	e.state = &store.State{}
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	return e
}

// dispatchAsync runs dispatch in the background.
func dispatchAsync(e *Engine, ctx context.Context, kind, arg string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.dispatch(ctx, kind, arg).err }()
	return done
}

func awaitError(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: dispatch still waiting", what)
		return nil
	}
}

// A request whose context ends stops waiting, both while the loop is busy
// and while the loop runs its command.
func TestDispatchReturnsWhenTheRequestEnds(t *testing.T) {
	e := newDispatchEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := dispatchAsync(e, ctx, "language", "en")
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := awaitError(t, done, "busy loop"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("busy loop: %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	done = dispatchAsync(e, ctx, "language", "en")
	cmd := <-e.cmdCh // taken, never answered
	cancel()
	if err := awaitError(t, done, "running command"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("running command: %v", err)
	}
	cmd.reply <- commandReply{}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := e.dispatch(ctx, "language", "en").err; !errors.Is(err, ErrCanceled) {
		t.Fatalf("ended context: %v", err)
	}
}

// Cancel answers the requests waiting for the loop at once, and no
// command is handed over after it.
func TestCancelEndsWaitingDispatch(t *testing.T) {
	e := newDispatchEngine(t)
	queued := dispatchAsync(e, context.Background(), "language", "en")
	running := dispatchAsync(e, context.Background(), "language", "en")
	// One of the two is taken by the "loop" and never answered; the other
	// waits for the loop.
	<-e.cmdCh
	time.Sleep(50 * time.Millisecond)
	e.Cancel()
	for _, done := range []<-chan error{queued, running} {
		if err := awaitError(t, done, "after Cancel"); !errors.Is(err, ErrStopped) {
			t.Fatalf("after Cancel: %v", err)
		}
	}
	// A request after Cancel does not reach cmdCh even when the loop takes
	// commands.
	taken := make(chan command, 1)
	go func() {
		select {
		case cmd := <-e.cmdCh:
			taken <- cmd
		case <-time.After(200 * time.Millisecond):
		}
	}()
	if err := e.dispatch(context.Background(), "language", "en").err; !errors.Is(err, ErrStopped) {
		t.Fatalf("after Cancel: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if len(taken) != 0 {
		t.Fatal("a command was handed to the loop after Cancel")
	}
}

// The loop's select can take a command after Cancel when both are ready:
// runCommand drops it.
func TestRunCommandAfterCancelIsDropped(t *testing.T) {
	e := newDispatchEngine(t)
	e.Cancel()
	cmd := command{kind: "language", arg: "en", reply: make(chan commandReply, 1)}
	e.runCommand(context.Background(), cmd)
	if reply := <-cmd.reply; !errors.Is(reply.err, ErrStopped) {
		t.Fatalf("reply %v", reply.err)
	}
	if e.state.Language != "" || e.dirty {
		t.Fatalf("the command ran: language %q dirty %v", e.state.Language, e.dirty)
	}
}

// Stop after Cancel still waits for the loop.
func TestStopAfterCancelWaitsForTheLoop(t *testing.T) {
	e := New(Options{Version: "test", DataDir: t.TempDir()})
	e.Start()
	e.Cancel()
	start := time.Now()
	e.Stop(3 * time.Second)
	select {
	case <-e.doneCh:
	default:
		t.Fatalf("Stop returned after %v with the loop running", time.Since(start))
	}
}
