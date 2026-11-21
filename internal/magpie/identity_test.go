package magpie

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// brokenSettings is a settings.json that cannot be read, so that the
// proxy of an account cannot be told.
var brokenSettings = map[string]string{"settings.json": "{"}

// The sign-in of agy's own account is confirmed before the proxy setting
// is read: an unreadable setting does not hide that agy is signed in to
// another account, or that its sign-in is in a keyring.
func TestAgyIdentityBeforeProxy(t *testing.T) {
	defer func(was string) { goos = was }(goos)
	for _, system := range []string{"linux", "darwin"} {
		goos = system
		h, home := agyFixture(t, "{}")
		writeFiles(t, h.ConfigDir, brokenSettings)
		writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agySignIn("ya29.other", "1//other", testNow.Add(time.Hour), "someone.else@example.org")})
		if _, err := h.Credential(context.Background(), agyOwn); !errors.Is(err, host.ErrIdentity) {
			t.Errorf("%s: Credential err = %v, want host.ErrIdentity", system, err)
		}
		if fp, err := h.ConfirmIdentity(context.Background(), agyOwn); !errors.Is(err, host.ErrIdentity) || fp != "" {
			t.Errorf("%s: ConfirmIdentity = %q, %v; want host.ErrIdentity", system, fp, err)
		}
	}
}

// ConfirmIdentity reads the sign-in alone: a confirmed account is
// confirmed while its proxy setting cannot be read, and no token is
// renewed for it.
func TestConfirmIdentityReadsNoProxy(t *testing.T) {
	defer func(was string) { goos = was }(goos)
	goos = "linux"
	h, home := agyFixture(t, agyToken("ya29.old", "1//refresh", testNow.Add(-time.Hour)))
	writeFiles(t, h.ConfigDir, brokenSettings)
	writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agySignIn("", "1//own", time.Time{}, "d@example.com")})
	g := &googleFake{status: 500}
	useGoogle(t, g)
	for _, c := range []struct {
		account host.Account
		want    string
	}{{agyAccount, fingerprint("1//refresh")}, {agyOwn, fingerprint("1//own")}} {
		if _, err := h.Credential(context.Background(), c.account); err == nil || errors.Is(err, host.ErrIdentity) {
			t.Errorf("%s: Credential err = %v, want the settings error", c.account.Pin, err)
		}
		if fp, err := h.ConfirmIdentity(context.Background(), c.account); err != nil || fp != c.want {
			t.Errorf("%s: ConfirmIdentity = %q, %v; want %q", c.account.Pin, fp, err, c.want)
		}
	}
	if len(g.calls) != 0 {
		t.Fatalf("renewed %d tokens", len(g.calls))
	}

	// A sign-in of another account in the account's home, and a home
	// without a sign-in, are not confirmed.
	h, _ = agyFixture(t, agySignIn("ya29.live", "1//refresh", testNow.Add(time.Hour), "someone.else@example.org"))
	if _, err := h.ConfirmIdentity(context.Background(), agyAccount); !errors.Is(err, errAgyOtherAccount) {
		t.Errorf("home of another account: err = %v", err)
	}
	h, _ = agyFixture(t, agySignIn("", "", testNow.Add(time.Hour), "c@example.com"))
	if _, err := h.ConfirmIdentity(context.Background(), agyAccount); err == nil {
		t.Error("a home without a sign-in was confirmed")
	}
	// Codex and Claude requests use the account's own credential.
	if fp, err := h.ConfirmIdentity(context.Background(), host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"}); err != nil || fp != "" {
		t.Errorf("Codex: %q, %v", fp, err)
	}
}
