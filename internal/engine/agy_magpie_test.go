package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/magpie"
)

// filesHost reads the agy-cli account "4" from Magpie's files with a
// magpie.Host, as the account pin, and serves the rest as the fakeHost.
type filesHost struct {
	*fakeHost
	files *magpie.Host
	pin   string
}

func (h *filesHost) magpieAccount() host.Account {
	return host.Account{Pin: h.pin, Provider: "agy-cli"}
}

func (h *filesHost) Credential(ctx context.Context, account host.Account) (host.Credential, error) {
	if account.Pin != "4" {
		return h.fakeHost.Credential(ctx, account)
	}
	return h.files.Credential(ctx, h.magpieAccount())
}

func (h *filesHost) ConfirmIdentity(ctx context.Context, _ host.Account) (string, error) {
	return h.files.ConfirmIdentity(ctx, h.magpieAccount())
}

// agyPin is the pin Magpie gives the agy-cli account named user.
func agyPin(user string) string {
	sum := sha256.Sum256([]byte("magpie login\nagy-cli\n" + strings.ToLower(user)))
	return "agy-cli/" + hex.EncodeToString(sum[:8])
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// agySignInFile is agy's token file signed in to email.
func agySignInFile(email string) string {
	b, _ := json.Marshal(map[string]any{
		"token":          map[string]any{"access_token": "ya29.x", "refresh_token": "1//x", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)},
		"auth_method":    "consumer",
		"id_token_email": email,
	})
	return string(b)
}

// agy's own account, d@example.com, while agy is signed in to another
// account (on macOS and Windows: while its sign-in is in the keyring),
// and Magpie's proxy setting cannot be read: no ignition request goes
// out. An account in a home of its own that is confirmed ignites with
// the same unreadable setting, as the gateway reads its proxy itself.
func TestAgyDefaultAccountWithUnreadableProxyDoesNotIgnite(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, shanghai)}
	h, e, _ := agyIgnitionHost(t, c)
	chosen := agyTarget(t, e)

	config, state, home := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", home)
	const agyHome = "0123456789abcdef"
	writeTestFile(t, filepath.Join(config, "logins.json"), `[
 {"agent":"plugin:agy-cli","user":"d@example.com","home":"agy-cli"},
 {"agent":"plugin:agy-cli","user":"c@example.com","home":"agy-cli#k3x9a2"}]`)
	writeTestFile(t, filepath.Join(config, "plugin-auth.json"), `{
 "agy-cli":{"type":"api","key":"agy","metadata":{"email":"d@example.com"}},
 "agy-cli#k3x9a2":{"type":"api","key":"agy:`+agyHome+`","metadata":{"email":"c@example.com","agyHome":"`+agyHome+`"}}}`)
	writeTestFile(t, filepath.Join(config, "settings.json"), "{")
	writeTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), agySignInFile("someone.else@example.org"))
	writeTestFile(t, filepath.Join(state, "magpie-agy-bridge", "accounts", agyHome, ".gemini", "antigravity-cli", "antigravity-oauth-token"), agySignInFile("c@example.com"))
	files := &filesHost{fakeHost: h, files: &magpie.Host{ConfigDir: config}, pin: agyPin("d@example.com")}
	e.host = files

	if _, err := files.Credential(context.Background(), host.Account{Pin: "4"}); !errors.Is(err, host.ErrIdentity) {
		t.Fatalf("the test needs an unconfirmed account: %v", err)
	}
	for _, manual := range []bool{true, false} {
		if err := e.igniteTarget(context.Background(), e.config(), chosen, manual); !errors.Is(err, host.ErrIdentity) || len(h.executed) != 0 {
			t.Fatalf("manual=%v: err=%v executed=%d", manual, err, len(h.executed))
		}
	}

	files.pin = agyPin("c@example.com")
	if _, err := files.Credential(context.Background(), host.Account{Pin: "4"}); err == nil || errors.Is(err, host.ErrIdentity) {
		t.Fatalf("the test needs a proxy setting that cannot be read: %v", err)
	}
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); errors.Is(err, host.ErrIdentity) || len(h.executed) != 1 {
		t.Fatalf("home account: err=%v executed=%d", err, len(h.executed))
	}
}
