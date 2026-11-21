package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
	"github.com/PosvdM/magpie-lamplighter/internal/store"
)

// leaky is an error text with an email, a URL with user info and query, a
// bearer credential, a JWT and a long token. leaked are its parts that no
// exit may show.
const leaky = `carol.secret@corp.example could not read https://u:pw@vendor.example/v1/usage?user=carol%40corp.example&sig=QSig ` +
	`with Bearer bEaReR.vAlUe eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln token ya29.A0b1C2d3E4f5G6h7I8j9K0l1M2n3O4p5Q6r7S8t9`

var leaked = []string{"carol.secret", "corp.example", "u:pw", "user=", "QSig", "bEaReR.vAlUe", "eyJhbGci", "eyJzdWIi", "A0b1C2d3E4f5G6h7I8j9"}

// showsNoSecret fails when one of texts shows a part of leaky.
func showsNoSecret(t *testing.T, where string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, part := range leaked {
			if strings.Contains(text, part) {
				t.Errorf("%s: %q shows %q", where, text, part)
			}
		}
	}
}

// eventTexts are the texts of events.
func eventTexts(events []Event) []string {
	var out []string
	for _, ev := range events {
		out = append(out, ev.Message, ev.Detail)
		for _, v := range ev.Params {
			out = append(out, v)
		}
	}
	return out
}

// A credential that cannot be read, as agy-cli's raw error, reaches the
// account's error of the status, the log and the answers of a manual
// refresh redacted.
func TestCredentialErrorIsRedactedAtTheExits(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	logs := &logHost{fakeHost: h}
	e.host = logs
	h.credErrs = map[string]error{"4": fmt.Errorf("%w: %s", host.ErrFormat, leaky)}
	e.poll(context.Background(), e.config(), c.t, "", false)
	found := false
	for _, a := range e.Status().Accounts {
		if a.AuthIndex == "4" {
			found = a.Error != ""
			showsNoSecret(t, "account error", a.Error)
		}
	}
	if !found {
		t.Fatal("account 4 has no error")
	}
	showsNoSecret(t, "log", logs.text())
	err := manualRefresh(e, "4")
	if !errors.Is(err, host.ErrFormat) {
		t.Fatalf("the error itself stays whole: %v", err)
	}
	result := mcpRefresh(t, e, "4")
	showsNoSecret(t, "refresh_quotas", fmt.Sprint(result))
}

// The account list's error reaches the status redacted.
func TestListErrorIsRedacted(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	logs := &logHost{fakeHost: h}
	e.host = failingList{logHost: logs, err: errors.New(leaky)}
	e.poll(context.Background(), e.config(), c.t, "", false)
	s := e.Status()
	if s.ListError == "" {
		t.Fatal("no list error")
	}
	showsNoSecret(t, "list error", s.ListError)
	showsNoSecret(t, "log", logs.text())
	e.noteModels(nil, errors.New(leaky))
	showsNoSecret(t, "models error", e.Status().ModelsError)
}

// failingList is a host whose account list fails with err.
type failingList struct {
	*logHost
	err error
}

func (f failingList) Accounts(context.Context) ([]host.Account, error) { return nil, f.err }

// A refused ignition's error reaches the events, their history, the
// target's state on disk, the credential problem and the notifications
// redacted, while the error stays whole for the classification.
func TestIgnitionErrorIsRedactedAtTheExits(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
			refused := &host.Error{Code: "model_execution_failed", Status: status,
				Message: `{"error":{"type":"authentication_error","message":"` + leaky + `"}}`}
			if status == http.StatusForbidden {
				refused.Message = `{"error":{"type":"permission_error","message":"` + leaky + `"}}`
			}
			h := claudeIgnitionHost(c, refused)
			dir := t.TempDir()
			e, sender := newTestEngineIn(t, h, c, dir)
			logs := &logHost{fakeHost: h}
			e.host = logs
			ctx := context.Background()
			cfg := e.config()
			cfg.Providers["codex"] = config.Provider{Monitor: true}
			e.Configure(cfg, nil)
			e.poll(ctx, cfg, c.t, "", false)
			e.performDue(ctx, cfg)
			if len(h.executed) != 1 {
				t.Fatalf("executed %d", len(h.executed))
			}
			e.saveState()

			s := e.Status()
			var failed []Event
			for _, ev := range s.Events {
				if ev.Kind == "ignite_failed" || ev.Kind == "ignite_paused" || ev.Kind == "credential_invalid" {
					failed = append(failed, ev)
				}
			}
			if len(failed) == 0 {
				t.Fatalf("no failure event in %+v", s.Events)
			}
			showsNoSecret(t, "events", eventTexts(s.Events)...)
			showsNoSecret(t, "in-memory events", eventTexts(e.events)...)
			for _, target := range s.Targets {
				showsNoSecret(t, "target", target.LastError, target.CircuitReason)
			}
			hist, err := e.HistoryRange(ctx, "24h", "5h")
			if err != nil {
				t.Fatal(err)
			}
			showsNoSecret(t, "history", eventTexts(hist.Events)...)
			for _, name := range []string{"state.json", "history"} {
				showsNoSecret(t, name, filesUnder(t, filepath.Join(dir, name))...)
			}
			if len(sender.sent) == 0 {
				t.Fatal("no notification")
			}
			for _, msg := range sender.sent {
				showsNoSecret(t, "notification", msg.Title, msg.Body)
			}
			showsNoSecret(t, "log", logs.text())
		})
	}
}

// filesUnder are the contents of the file path, or of the files in the
// directory path.
func filesUnder(t *testing.T, path string) []string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return []string{string(b)}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, filesUnder(t, filepath.Join(path, entry.Name()))...)
	}
	return out
}

// Events kept before redaction, in the history and in memory, leave the
// status and the history redacted.
func TestOldEventsAreRedactedWhenRead(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	dir := t.TempDir()
	e, _ := newTestEngineIn(t, h, c, dir)
	e.mu.Lock()
	e.events = append(e.events, Event{Time: c.t, Level: "warn", Kind: "ignite_failed", Message: leaky, Detail: leaky, Params: map[string]string{"error": leaky}})
	e.mu.Unlock()
	s := e.Status()
	showsNoSecret(t, "status events", eventTexts(s.Events)...)
	if e.events[len(e.events)-1].Params["error"] != leaky {
		t.Fatal("the status changed the engine's own events")
	}

	// A history record written before redaction.
	old := store.Record{Kind: "e", Time: c.t.Add(-time.Hour).Unix(), Group: "claude:3:claude:main", Event: "ignite_failed",
		Level: "warn", Message: leaky, Detail: leaky, Params: map[string]string{"error": leaky}}
	if err := e.history.Append(old); err != nil {
		t.Fatal(err)
	}
	hist, err := e.HistoryRange(context.Background(), "24h", "5h")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Events) == 0 {
		t.Fatal("the old record was not read")
	}
	showsNoSecret(t, "history", eventTexts(hist.Events)...)
	again, _ := newTestEngineIn(t, h, c, dir)
	again.loadRecentEvents()
	if len(again.events) == 0 {
		t.Fatal("no recent events loaded")
	}
	showsNoSecret(t, "loaded events", eventTexts(again.events)...)
}

// The record of an ignition stream is logged redacted, the reply too.
func TestIgnitionRecordIsRedacted(t *testing.T) {
	h := newStreamHost()
	reply, _ := json.Marshal(leaky)
	h.event(`{"type":"response.output_text.delta","delta":` + string(reply) + `}`)
	h.event(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`)
	e := New(Options{Host: h})
	if _, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow); err != nil {
		t.Fatal(err)
	}
	r := streamRecord(t, h)
	if r["reply"] == "" {
		t.Fatal(r)
	}
	showsNoSecret(t, "ignition record", h.messages...)
}

// The reason of a stale refresh and the 4xx body of Magpie's gateway reach
// the account's error, the log and refresh_quotas redacted.
func TestRefreshErrorsAreRedactedAtTheExits(t *testing.T) {
	e, h, renew, c := newRenewEngine(t)
	logs := &logHost{fakeHost: h}
	renew.expiredHost = &expiredHost{fakeHost: h, pin: "3"}
	c.add(time.Minute)
	h.kept = []host.KeptQuota{keptClaude("3", c.t.Add(-30*time.Second), 47)}
	renew.refresh = func(context.Context, string) error {
		return &host.Error{Method: "RefreshQuota", Code: "refresh_stale", Message: leaky}
	}
	e.host = &loggedRenewHost{renewHost: renew, logs: logs}
	err := manualRefresh(e, "3")
	if err == nil {
		t.Fatal("no error")
	}
	for _, a := range e.Status().Accounts {
		showsNoSecret(t, "account error", a.Error)
	}
	c.add(time.Minute)
	result := mcpRefresh(t, e, "3")
	if result["ok"] != false || !strings.HasPrefix(fmt.Sprint(result["error"]), "Magpie 这次没有读到新额度") {
		t.Fatalf("refresh_quotas %#v", result)
	}
	showsNoSecret(t, "refresh_quotas", fmt.Sprint(result))
	showsNoSecret(t, "log", logs.text())

	// The gateway answers the refresh with a 4xx error whose body quotes
	// the account and a credential.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/magpie/accounts":
			w.Write([]byte(`{"object":"list","data":[{"provider":"claude","id":"3","pin":"claude/3","kind":"account","user":"carol.secret@corp.example","on":true}]}`))
		case "GET /v1/magpie/quotas":
			w.Write([]byte(`{"object":"list","data":[]}`))
		default:
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"message":"` + leaky + `"}}`))
		}
	}))
	t.Cleanup(gateway.Close)
	logged := &loggedGatewayHost{gatewayRefreshHost: &gatewayRefreshHost{
		expiredHost: renew.expiredHost,
		gateway:     &magpie.Host{Addr: strings.TrimPrefix(gateway.URL, "http://"), ConfigDir: t.TempDir()},
	}}
	e.host = logged
	c.add(time.Minute)
	err = manualRefresh(e, "3")
	var hostErr *host.Error
	if !errors.As(err, &hostErr) || hostErr.Status != http.StatusForbidden {
		t.Fatalf("the error itself stays whole: %v", err)
	}
	for _, a := range e.Status().Accounts {
		showsNoSecret(t, "account error after a 4xx", a.Error)
	}
	logged.mu.Lock()
	showsNoSecret(t, "log after a 4xx", logged.lines...)
	logged.mu.Unlock()
}

// loggedRenewHost is a renewHost that records the log in logs.
type loggedRenewHost struct {
	*renewHost
	logs *logHost
}

func (h *loggedRenewHost) Log(level, message string, fields map[string]any) {
	h.logs.Log(level, message, fields)
}

// The result of a test notification leaves the error of a channel
// redacted.
func TestTestNotifyErrorIsRedacted(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, leaky, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	cfg := e.config()
	cfg.BarkURL = srv.URL + "/devicekey"
	cfg.Webhook = config.Webhook{URL: srv.URL + "/hook", Method: "POST"}
	e.Configure(cfg, nil)
	e.applyRuntimeConfig(cfg)
	results, err := e.testNotify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].OK || results[1].OK {
		t.Fatalf("results %+v", results)
	}
	for _, r := range results {
		showsNoSecret(t, "test notification", r.Error, r.Response)
	}

	// Webhook settings that were not applied.
	e.mu.Lock()
	e.notifyErr = leaky
	e.mu.Unlock()
	results, err = e.testNotify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		showsNoSecret(t, "test notification", r.Error, r.Response)
	}
	showsNoSecret(t, "status", e.Status().NotifyError)
}
