package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/providers"
)

// authEntry is an account the fake host lists. AuthIndex is its pin, and
// Note its Magpie label.
type authEntry struct {
	ID, AuthIndex, Name, Provider, Email, Note, User string
	Disabled                                         bool
}

func (a authEntry) account() host.Account {
	return host.Account{Pin: a.AuthIndex, Provider: a.Provider, Label: a.Note, Email: a.Email, User: a.User, On: !a.Disabled}
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
		ProjectID:   str("project_id"),
		ProxyURL:    str("proxy_url"),
		Data:        data,
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
