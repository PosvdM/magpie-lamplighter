package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// authEntry is an account the fake host lists. AuthIndex is its pin, and
// Note its Magpie label.
type authEntry struct {
	ID, AuthIndex, Name, Provider, Email, Note, User, Plan string
	Disabled                                               bool
}

func (a authEntry) account() host.Account {
	return host.Account{Pin: a.AuthIndex, Provider: a.Provider, Label: a.Note, Email: a.Email, User: a.User, Plan: a.Plan, On: !a.Disabled}
}

// httpRequest and httpResponse are the quota requests the fake vendor sees.
type httpRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

type httpResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// usageRecord is a passive reading in the form of response headers.
type usageRecord struct {
	Provider        string
	AuthIndex       string
	ResponseHeaders http.Header
	RequestedAt     time.Time
}

// observeHeaders parses rec with its provider's header parser and queues it.
func observeHeaders(e *Engine, rec usageRecord) {
	p := providers.Lookup(rec.Provider)
	if p == nil || p.Headers == nil {
		return
	}
	e.ObservePassive(rec.AuthIndex, p.Headers(rec.ResponseHeaders, rec.RequestedAt), rec.RequestedAt)
}

// applyHeaders applies a passive reading of headers at once.
func applyHeaders(e *Engine, ctx context.Context, provider, authIndex string, at time.Time, header http.Header) {
	if at.IsZero() {
		at = e.now()
	}
	e.applyUsage(ctx, usageEvent{authIndex: authIndex, groups: providers.Lookup(provider).Headers(header, at), at: at})
}

// credentialFromFile reads a credential from the JSON the tests write for an
// account. The fingerprint hashes the refresh token, else the API key, else
// the access token.
func credentialFromFile(raw string) host.Credential {
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return host.Credential{}
	}
	str := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	cred := host.Credential{
		AccessToken: str("access_token"),
		AccountID:   str("account_id"),
		ProxyURL:    str("proxy_url"),
	}
	for _, key := range []string{"refresh_token", "api_key", "access_token"} {
		if value := str(key); value != "" {
			sum := sha256.Sum256([]byte(key + "\x00" + value))
			cred.Fingerprint = hex.EncodeToString(sum[:8])
			break
		}
	}
	return cred
}

// fingerprintOf is the fingerprint of the credential in raw.
func fingerprintOf(raw json.RawMessage) string { return credentialFromFile(string(raw)).Fingerprint }

// agyReading is Magpie's reading of the agy-cli account pin, read at
// readAt, as package magpie converts it.
func agyReading(pin string, readAt time.Time, groups ...quota.RawGroup) host.KeptQuota {
	return host.KeptQuota{Pin: pin, Groups: groups, ReadAt: readAt, Active: true}
}

// agyPool is the group of an Antigravity pool, such as "Gemini" or
// "Claude & GPT", with a 5-hour window of remaining percent that resets at
// reset.
func agyPool(pool string, remaining float64, reset time.Time) quota.RawGroup {
	g, _ := providers.AntigravityPool(pool)
	return quota.RawGroup{Key: g.Key, SourceLabel: g.SourceLabel, Windows: []quota.Window{
		{ID: g.FiveHour, Label: quota.LabelFiveHour, Remaining: remaining, Reset: reset, Period: quota.FiveHours},
	}}
}

// withWeekly adds the 7-day window of the pool of g, with remaining percent
// and reset.
func withWeekly(g quota.RawGroup, pool string, remaining float64, reset time.Time) quota.RawGroup {
	p, _ := providers.AntigravityPool(pool)
	g.Windows = append(append([]quota.Window(nil), g.Windows...), quota.Window{ID: p.Weekly, Label: quota.LabelSevenDay, Remaining: remaining, Reset: reset, Period: quota.SevenDays})
	return g
}

// logHost records the log lines of the engine.
type logHost struct {
	*fakeHost
	mu    sync.Mutex
	lines []string
}

func (h *logHost) Log(level, message string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, fmt.Sprint(level, " ", message, " ", fields))
}

func (h *logHost) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}
