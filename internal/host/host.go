// Package host is the contract between Lamplighter and Magpie.
//
// The engine sees Magpie only through Host. Package magpie implements it
// against a running Magpie: the account list and quotas from the gateway at
// MAGPIE_ADDR, the Codex and Claude credentials from Magpie's configuration
// directory, the agy-cli sign-ins that ignition confirms from local files,
// and ignition requests through the gateway with X-Magpie-Account. Tests
// use a fake Host.
package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// Error is a failed host call or a normalized upstream error. Status carries
// the HTTP status, or 0 when none is known.
type Error struct {
	Method  string
	Code    string
	Message string
	Status  int
	// AppealURL is the appeal form of a suspended account. Only the
	// normalized stream errors set it, from an allowlisted address.
	AppealURL string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var hostErr *Error
	if errors.As(err, &hostErr) {
		return hostErr.Status
	}
	return 0
}

// ErrNotImplemented is returned by the parts of a Host that are not written
// yet.
var ErrNotImplemented = errors.New("not implemented")

// ErrFormat reports a Magpie file or response whose format Lamplighter does
// not know. Implementations wrap it and never guess a field.
var ErrFormat = errors.New("unknown Magpie format")

// ErrIdentity is wrapped by ConfirmIdentity when it cannot confirm that
// the sign-in the gateway sends an account's requests with belongs to the
// account: agy's own sign-in of an agy-cli account holds whichever Google
// account agy is signed in to. Ignition sends no request for such an
// account; see docs/architecture.md.
var ErrIdentity = errors.New("credential of another or an unconfirmed account")

// Account is one subscription account that Magpie serves.
type Account struct {
	// Pin is Magpie's "<provider>/<id>" from GET /v1/magpie/accounts. It is
	// the stable account key in state.json, account_weights, account_order
	// and account_names, and the value of X-Magpie-Account.
	Pin string `json:"pin"`
	// Provider is Magpie's provider id, such as "codex", "claude" or
	// "agy-cli". The engine maps it to a Lamplighter provider with
	// providers.Lookup.
	Provider string `json:"provider"`
	// Label is the name Magpie shows; Email is the account's email when
	// Magpie knows one.
	Label string `json:"label,omitempty"`
	Email string `json:"email,omitempty"`
	// User is Magpie's name of the account, the user of GET
	// /v1/magpie/accounts as it is: the email, and for a seat in a Team or
	// a similar workspace the workspace too, such as "a@b.com · Team".
	// Magpie's per-account settings are keyed by it in lower case.
	User string `json:"user,omitempty"`
	// Plan is Magpie's plan name, for display only.
	Plan string `json:"plan,omitempty"`
	// On is false for an account the user switched off in Magpie.
	On bool `json:"on"`
}

// Credential is what a provider needs to read the quota of a Codex or
// Claude account. Implementations read it from Magpie's files and never
// refresh a token; see docs/architecture.md. Antigravity has none: its
// quota is read only from Magpie's quota cards, and its sign-in is only
// confirmed (IdentityChecker).
type Credential struct {
	// Provider is Magpie's provider id; ID and Name are the account pin.
	Provider    string
	ID          string
	Name        string
	AccessToken string
	// AccountID is the ChatGPT account id of a Codex account.
	AccountID string
	// ProxyURL is the proxy Magpie uses for this account's requests: the
	// account's or provider's own, else Magpie's global proxy. "" means
	// the environment proxy or a direct connection, "direct" a direct one.
	ProxyURL string
	// Fingerprint identifies the login, such as a hash of the refresh
	// token; it changes when the user signs in again. "" when unknown.
	Fingerprint string
	// Expired is set when the access token is known to be past its expiry
	// and cannot be renewed by Lamplighter. The engine then reads Magpie's
	// kept quota for the account instead of asking the vendor.
	Expired bool
	// RenewedByRefresh is set with Expired when Magpie renews the token
	// as it refreshes the account's quota (RefreshQuota): a Claude
	// sign-in, whose refresh runs Claude Code, and a Codex sign-in. A
	// Claude setup-token is not.
	RenewedByRefresh bool
}

// KeptQuota is Magpie's last reading of one account's quota, converted to
// Lamplighter groups. Magpie sends no request to read it. The groups and
// windows use the keys and IDs of the provider's own Fetch, such as
// "claude:main" and quota.WindowFiveHour, so that they update the same
// windows; a window the active query has not reported is ignored. An
// Antigravity reading uses the keys and IDs of providers.AntigravityPool.
type KeptQuota struct {
	Pin    string
	Groups []quota.RawGroup
	// ReadAt is when Magpie took the reading.
	ReadAt time.Time
	// Active is set on a reading Magpie made of the whole account: a
	// group missing from it, other than one of Unread, no longer exists,
	// and the engine applies each of Groups as an active query. Only an
	// Antigravity reading is active; it has at least one group.
	Active bool
	// Unread lists the keys of the groups of an active reading that the
	// account still has but that the reading gave no figure for, such as
	// an Antigravity pool agy shows as "Limits not available". The engine
	// leaves them as they are.
	Unread []string
	// Plan is the plan Magpie's account list gave the account of an
	// active reading when the reading was read, "" when it gave none. The
	// engine records it before it applies the reading.
	Plan string
	// PlanAt is when the host began to read the account list Plan comes
	// from. The engine records Plan only when no later list has given
	// the account a plan.
	PlanAt time.Time
}

// ModelRequest is one ignition request through the Magpie gateway.
type ModelRequest struct {
	// Account is the pin sent as X-Magpie-Account.
	Account string
	// Protocol is the request format: "openai-response" (POST
	// /v1/responses), "claude" (POST /v1/messages) or "openai" (POST
	// /v1/chat/completions).
	Protocol string
	Model    string
	// Body is the JSON request body with stream set.
	Body []byte
}

// ModelStream is an opened streaming response.
type ModelStream struct {
	ID         string
	StatusCode int
	Header     http.Header
}

// StreamChunk is the next part of a stream. Done marks the end of the body.
type StreamChunk struct {
	Payload []byte
	Error   string
	Done    bool
}

// ModelResponse is what the engine keeps of an ignition response to
// confirm the new window.
type ModelResponse struct {
	StatusCode int
	Header     http.Header
}

// Host is everything Lamplighter asks of Magpie.
type Host interface {
	// Accounts lists the subscription accounts.
	Accounts(ctx context.Context) ([]Account, error)
	// Credential reads the credential of one account.
	Credential(ctx context.Context, account Account) (Credential, error)
	// KeptQuotas returns Magpie's last quota readings: the passive data,
	// and the fallback for an account whose token has expired.
	KeptQuotas(ctx context.Context) ([]KeptQuota, error)
	// RefreshQuota asks Magpie to read one account's quota again. For
	// Claude this runs Claude Code's /usage, and for Antigravity agy's
	// /usage, so callers limit it.
	RefreshQuota(ctx context.Context, pin string) error
	// Models lists the model ids the gateway serves.
	Models(ctx context.Context) ([]string, error)
	// OpenModelStream sends an ignition request and returns once the
	// response headers arrive. Cancelling ctx cancels the request only
	// until then: a response the provider has started is not cut by it,
	// and the body of an error status is read within a bounded wait of
	// its own. After the headers only CloseModelStream ends the request,
	// and the caller closes every stream it opened.
	OpenModelStream(ctx context.Context, req ModelRequest) (ModelStream, error)
	// ReadModelStream waits for the next chunk. CloseModelStream may run
	// concurrently and unblocks it.
	ReadModelStream(id string) (StreamChunk, error)
	CloseModelStream(id string) error
	// Log writes one log entry.
	Log(level, message string, fields map[string]any)
}

// IdentityChecker is implemented by a Host that can confirm, from local
// files alone, whose sign-in the gateway will send an account's requests
// with. Ignition of an agy-cli account calls it before every request, as
// agy can sign in to another Google account at any time; without it no
// agy-cli account is confirmed, and none is ignited. Every error means the
// account is not confirmed, and no request is sent.
type IdentityChecker interface {
	// ConfirmIdentity returns the fingerprint of the account's sign-in
	// once the sign-in is confirmed to be the account's. It reads local
	// files only: it sends no request, reads no proxy setting and renews
	// no token.
	ConfirmIdentity(ctx context.Context, account Account) (fingerprint string, err error)
}

// KeptRefresher is implemented by a Host whose RefreshQuota answers the
// account's quota card: RefreshKept is RefreshQuota that also returns
// that card as a KeptQuota, so that the caller need not read KeptQuotas
// again. found is false when the answer has no card of the account to
// use, as when the call failed before the refresh was answered; the error
// is the one RefreshQuota returns, and a card can come with an error that
// says the reading is not new.
type KeptRefresher interface {
	RefreshKept(ctx context.Context, pin string) (kept KeptQuota, found bool, err error)
}

type confirmingKey struct{}

// ConfirmingIgnition marks ctx as that of a RefreshQuota or RefreshKept
// that confirms an ignition. The engine sends at most a few such refreshes
// per ignition, so a host may let them through within the refresh gap of
// an account whose quota it reads only when asked.
func ConfirmingIgnition(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmingKey{}, true)
}

// IsConfirmingIgnition reports whether ctx was marked by ConfirmingIgnition.
func IsConfirmingIgnition(ctx context.Context) bool {
	v, _ := ctx.Value(confirmingKey{}).(bool)
	return v
}

// ErrRefreshLimited is the error of a RefreshQuota or RefreshKept that a
// host refused without sending the refresh, because the account's quota
// was refreshed too recently.
var ErrRefreshLimited = errors.New("额度刷新过于频繁")

type sendingKey struct{}

// WithRefreshSending returns ctx for a RefreshQuota or RefreshKept that
// calls sending right before the host sends the refresh, with the time
// the host counts its refresh gap from. An error of sending stops the
// refresh unsent, and RefreshQuota or RefreshKept returns that error.
func WithRefreshSending(ctx context.Context, sending func(at time.Time) error) context.Context {
	return context.WithValue(ctx, sendingKey{}, sending)
}

// RefreshSending is called by a host right before it sends the refresh
// of a RefreshQuota or RefreshKept with ctx, at at; see
// WithRefreshSending. It returns nil for a ctx without one.
func RefreshSending(ctx context.Context, at time.Time) error {
	if sending, _ := ctx.Value(sendingKey{}).(func(time.Time) error); sending != nil {
		return sending(at)
	}
	return nil
}
