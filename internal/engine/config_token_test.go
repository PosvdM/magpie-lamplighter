package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/mcp"
	"github.com/PosvdM/magpie-lamplighter/internal/notify"
)

func TestConfigAppliedTokenWaitsForRuntimeAndMCP(t *testing.T) {
	for _, channel := range []string{"bark", "webhook"} {
		t.Run(channel, func(t *testing.T) {
			e := New(Options{})
			cfg := config.Default()
			cfg.ConfigToken = "old"
			cfg.BarkURL = "https://example.com/old-bark"
			cfg.Webhook.URL = "https://example.com/old-webhook"
			rev := e.Configure(cfg, nil)
			e.MCPConfigured(rev)
			applied, _ := e.applyPendingConfig(config.Default())
			if e.Status().ConfigAppliedToken != "old" {
				t.Fatal("initial token not acknowledged")
			}
			cfg.ConfigToken = "new"
			if channel == "bark" {
				cfg.BarkURL = "https://example.com/new-bark"
			} else {
				cfg.Webhook.URL = "https://example.com/new-webhook"
			}
			rev = e.Configure(cfg, nil)
			e.MCPConfigured(rev)
			if got := e.Status().ConfigAppliedToken; got != "old" {
				t.Fatalf("received config acknowledged before runtime apply: %s", got)
			}
			if e.alerts.Cfg.BarkURL != applied.BarkURL || e.alerts.Cfg.Webhook.URL != applied.Webhook.URL {
				t.Fatal("runtime changed before loop")
			}
			e.applyPendingConfig(applied)
			if e.Status().ConfigAppliedToken != "new" {
				t.Fatal("applied token did not advance")
			}
			fanout := e.alerts.Sender.(*notify.Fanout)
			if fanout.Channels[0].Sender.(*notify.Bark).URL != cfg.BarkURL {
				t.Fatal("Bark did not use new runtime settings")
			}
			if e.alerts.Cfg.Webhook.URL != cfg.Webhook.URL {
				t.Fatal("webhook did not use new runtime settings")
			}
			cfg.ConfigToken = "runtime-first"
			rev = e.Configure(cfg, nil)
			e.applyPendingConfig(cfg)
			if e.Status().ConfigAppliedToken != "new" {
				t.Fatal("runtime alone must not acknowledge MCP")
			}
			e.MCPConfigured(rev)
			if e.Status().ConfigAppliedToken != cfg.ConfigToken {
				t.Fatal("MCP completion must acknowledge the same revision")
			}
		})
	}
}

func TestConfigTokenParseFailureAndSupersededRevision(t *testing.T) {
	e := New(Options{})
	cfg, err := config.Parse([]byte("config_token: accepted\n"))
	if err != nil || cfg.ConfigToken != "accepted" {
		t.Fatalf("parse: %+v %v", cfg, err)
	}
	rev := e.Configure(cfg, nil)
	e.MCPConfigured(rev)
	e.applyPendingConfig(config.Default())
	invalid, err := config.Parse([]byte("config_token: rejected\nproviders: [\n"))
	if err == nil {
		t.Fatal("expected parse failure")
	}
	e.Configure(invalid, err)
	e.applyPendingConfig(cfg)
	if got := e.Status().ConfigAppliedToken; got != "accepted" {
		t.Fatalf("parse failure advanced token: %s", got)
	}
	cfg.ConfigToken = "superseded"
	older := e.Configure(cfg, nil)
	snapshot, generation := e.configSnapshot()
	cfg.ConfigToken = "latest"
	latest := e.Configure(cfg, nil)
	e.applyRuntimeConfig(snapshot)
	e.confirmRuntimeConfig(generation, snapshot.ConfigToken)
	e.MCPConfigured(older)
	if e.Status().ConfigAppliedToken != "superseded" {
		t.Fatal("the latest fully completed snapshot must be acknowledged without claiming the pending configuration")
	}
	e.MCPConfigured(latest)
	e.applyPendingConfig(snapshot)
	if e.Status().ConfigAppliedToken != "latest" {
		t.Fatal("latest revision not acknowledged")
	}
}

func freeMCPAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}

func TestMCPOnlyConfigAdvancesTokenAndRotatesAuthentication(t *testing.T) {
	e := New(Options{})
	server := mcp.New(NewMCPBackend(e), "test")
	defer server.Close()
	cfg := config.Default()
	cfg.MCPEnabled, cfg.PublicListen, cfg.MCPKey, cfg.ConfigToken = true, freeMCPAddress(t), "old-key", "old"
	configure := func() {
		rev := e.Configure(cfg, nil)
		server.Configure(mcp.Config{Listen: cfg.PublicListen, MCPEnabled: cfg.MCPEnabled, MCPKey: cfg.MCPKey})
		e.MCPConfigured(rev)
	}
	configure()
	applied, _ := e.applyPendingConfig(config.Default())
	alerts := e.alerts
	cfg.MCPKey, cfg.ConfigToken = "new-key", "new"
	configure()
	if got := e.Status().ConfigAppliedToken; got != "old" {
		t.Fatalf("MCP applied before engine confirmation: %s", got)
	}
	e.applyPendingConfig(applied)
	if got := e.Status().ConfigAppliedToken; got != "new" || e.alerts != alerts {
		t.Fatalf("MCP-only update: token=%s alerts rebuilt=%v", got, e.alerts != alerts)
	}
	for _, tc := range []struct {
		key    string
		status int
	}{{"old-key", 401}, {"new-key", 200}} {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://"+cfg.PublicListen+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tc.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("key=%s status=%d", tc.key, resp.StatusCode)
		}
	}
}

// The keys the page writes for its own state change neither the runtime
// config (notification channels, timeouts, time zone) nor the poll interval,
// so saving them only confirms the new token.
// A save of only the page's state updates the config the status shows,
// without waking the background loop, which would run the ignitions due
// within its lead early, or moving the applied token.
func TestPageStateLeavesTheLoopAsleep(t *testing.T) {
	e := New(Options{})
	before := config.Default()
	before.ConfigToken = "settings"
	e.MCPConfigured(e.Configure(before, nil))
	e.applyPendingConfig(config.Default())
	<-e.wakeCh
	revision := e.cfgRevision
	cfg := before
	cfg.BarkURL = "https://example.com/not-applied"
	cfg.RememberChartFilters = true
	cfg.RememberChartExpanded = true
	cfg.ChartView = &config.ChartView{Window: "7d", Sort: "low", Ranges: map[string]string{"7d": "15d"}}
	cfg.ChartExpanded = []string{"claude|Claude"}
	cfg.MCPPublicURL = "https://mcp.example.com"
	cfg.MaskAccounts = true
	cfg.EventsShow5hResets = true
	if !e.ConfigurePageState(cfg) {
		t.Fatal("page state refused")
	}
	select {
	case <-e.wakeCh:
		t.Fatal("page state woke the background loop")
	default:
	}
	got := e.Status()
	if e.cfgRevision != revision || got.ConfigAppliedToken != "settings" {
		t.Fatalf("revision %d, applied token %q", e.cfgRevision, got.ConfigAppliedToken)
	}
	want := before
	want.SetPageState(cfg)
	if !reflect.DeepEqual(e.config(), want) || e.config().BarkURL != "" {
		t.Fatalf("config %+v", e.config())
	}
	// After a config that did not parse, the next save goes through
	// Configure, which clears the error.
	e.Configure(config.Config{}, errors.New("bad"))
	if e.ConfigurePageState(cfg) {
		t.Fatal("page state taken over a config error")
	}
}
