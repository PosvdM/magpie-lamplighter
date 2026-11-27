package magpie

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// agyFixture sets up the agy-cli plugin's accounts, as agy-cli 0.1.151
// keeps them: "c@example.com" with a home of its own, whose token file
// holds token, and "d@example.com" agy's own. It returns the user's home,
// where agy keeps its own account's file.
func agyFixture(t *testing.T, token string) (*Host, string) {
	t.Helper()
	useClock(t)
	h := &Host{ConfigDir: t.TempDir()}
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", home)
	writeFiles(t, h.ConfigDir, map[string]string{
		"logins.json": loginsFile("", "{}"),
		"plugin-auth.json": mustJSON(map[string]any{
			"agy-cli":        map[string]any{"type": "api", "key": "agy", "metadata": map[string]any{"email": "d@example.com"}},
			"agy-cli#k3x9a2": map[string]any{"type": "api", "key": "agy:0123456789abcdef", "metadata": map[string]any{"email": "c@example.com", "agyHome": "0123456789abcdef"}},
		}),
	})
	writeFiles(t, state, map[string]string{"magpie-agy-bridge/accounts/0123456789abcdef/.gemini/antigravity-cli/antigravity-oauth-token": token})
	return h, home
}

// agyToken is a sign-in of c@example.com, the account with a home.
func agyToken(access, refresh string, expiry time.Time) string {
	return agySignIn(access, refresh, expiry, "c@example.com")
}

// agySignIn is agy's token file signed in to email, in agy-cli 0.1.151's
// shape; "" leaves out id_token_email.
func agySignIn(access, refresh string, expiry time.Time, email string) string {
	v := map[string]any{"token": map[string]any{
		"access_token": access, "token_type": "Bearer", "refresh_token": refresh, "expiry": expiry.Format(time.RFC3339Nano), "expires_in": 3599,
	}, "auth_method": "consumer"}
	if email != "" {
		v["id_token_email"] = email
	}
	return mustJSON(v)
}

var (
	agyAccount = host.Account{Pin: "agy-cli/" + nameID("agy-cli", "c@example.com"), Provider: "agy-cli"}
	agyOwn     = host.Account{Pin: "agy-cli/" + nameID("agy-cli", "d@example.com"), Provider: "agy-cli"}
)

// The sign-in of an account with a home confirms the account, with the
// fingerprint of its refresh token, else of its access token, in each of
// the forms go-keyring writes.
func TestAgyIdentity(t *testing.T) {
	token := agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour))
	for name, file := range map[string]string{
		"JSON":               token,
		"go-keyring-base64":  "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(token)),
		"go-keyring-encoded": "go-keyring-encoded:" + hex.EncodeToString([]byte(token)),
	} {
		h, _ := agyFixture(t, file)
		if fp, err := h.ConfirmIdentity(context.Background(), agyAccount); err != nil || fp != fingerprint("1//refresh") {
			t.Errorf("%s: ConfirmIdentity = %q, %v", name, fp, err)
		}
	}
	h, _ := agyFixture(t, agyToken("ya29.only", "", testNow.Add(time.Hour)))
	if fp, err := h.ConfirmIdentity(context.Background(), agyAccount); err != nil || fp != fingerprint("ya29.only") {
		t.Errorf("access token only: %q, %v", fp, err)
	}
	// Codex and Claude requests use the account's own credential.
	if fp, err := h.ConfirmIdentity(context.Background(), host.Account{Pin: "codex/1a2b3c4d5e6f7a8b", Provider: "codex"}); err != nil || fp != "" {
		t.Errorf("Codex: %q, %v", fp, err)
	}
}

// Credential gives no token of an agy-cli account, whose sign-in only
// ConfirmIdentity reads.
func TestAgyCredentialIsNotRead(t *testing.T) {
	h, _ := agyFixture(t, agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour)))
	if got, err := h.Credential(context.Background(), agyAccount); err == nil || got.AccessToken != "" || got.Fingerprint != "" {
		t.Fatalf("agy-cli credential: %+v, %v", got, err)
	}
}

// ConfirmIdentity reads the sign-in alone: a confirmed account is
// confirmed while its proxy setting cannot be read.
func TestConfirmIdentityReadsNoProxy(t *testing.T) {
	defer func(was string) { goos = was }(goos)
	goos = "linux"
	h, home := agyFixture(t, agyToken("ya29.old", "1//refresh", testNow.Add(-time.Hour)))
	writeFiles(t, h.ConfigDir, map[string]string{"settings.json": "{"})
	writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agySignIn("", "1//own", time.Time{}, "d@example.com")})
	for _, c := range []struct {
		account host.Account
		want    string
	}{{agyAccount, fingerprint("1//refresh")}, {agyOwn, fingerprint("1//own")}} {
		if fp, err := h.ConfirmIdentity(context.Background(), c.account); err != nil || fp != c.want {
			t.Errorf("%s: ConfirmIdentity = %q, %v; want %q", c.account.Pin, fp, err, c.want)
		}
	}
	// A home without a sign-in is not confirmed.
	h, _ = agyFixture(t, agySignIn("", "", testNow.Add(time.Hour), "c@example.com"))
	if _, err := h.ConfirmIdentity(context.Background(), agyAccount); err == nil {
		t.Error("a home without a sign-in was confirmed")
	}
}

// agy's own account is in the keyring on macOS and Windows, which
// Lamplighter does not read, and in ~/.gemini on Linux without one.
func TestAgyOwnAccount(t *testing.T) {
	h, home := agyFixture(t, "{}")
	defer func(was string) { goos = was }(goos)
	for _, system := range []string{"darwin", "windows"} {
		goos = system
		if _, err := h.ConfirmIdentity(context.Background(), agyOwn); !errors.Is(err, errAgyKeyring) || !errors.Is(err, host.ErrIdentity) {
			t.Fatalf("%s: err = %v, want errAgyKeyring", system, err)
		}
	}
	goos = "linux"
	for _, email := range []string{"d@example.com", " D@Example.COM "} {
		writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agySignIn("ya29.own", "1//own", testNow.Add(time.Hour), email)})
		if fp, err := h.ConfirmIdentity(context.Background(), agyOwn); err != nil || fp != fingerprint("1//own") {
			t.Fatalf("linux, signed in to %q: %q, %v", email, fp, err)
		}
	}
}

// agy's default file holds whichever account agy is signed in to now.
// After agy signs in to another Google account, or when the file or the
// entry does not say whose it is, agy's own account is not confirmed, and
// the error names neither email in full.
func TestAgyOwnAccountSwitched(t *testing.T) {
	defer func(was string) { goos = was }(goos)
	goos = "linux"
	for _, c := range []struct {
		name, signedIn string
		entry          map[string]any
		want           error
	}{
		{"agy signed in to another account", "someone.else@example.org", nil, errAgyOtherAccount},
		{"file without id_token_email", "", nil, errAgyUnproven},
		{"entry without email", "d@example.com", map[string]any{"type": "api", "key": "agy"}, errAgyUnproven},
	} {
		h, home := agyFixture(t, "{}")
		account := agyOwn
		if c.entry != nil {
			writeFiles(t, h.ConfigDir, map[string]string{
				"logins.json":      `[{"agent":"plugin:agy-cli","user":"Signed in","home":"agy-cli"}]`,
				"plugin-auth.json": mustJSON(map[string]any{"agy-cli": c.entry}),
			})
			account = host.Account{Pin: "agy-cli/" + nameID("agy-cli", "Signed in"), Provider: "agy-cli"}
		}
		writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agySignIn("ya29.other", "1//other", testNow.Add(-time.Hour), c.signedIn)})
		fp, err := h.ConfirmIdentity(context.Background(), account)
		if !errors.Is(err, c.want) || !errors.Is(err, host.ErrIdentity) || errors.Is(err, host.ErrFormat) || fp != "" {
			t.Errorf("%s: %q, err = %v, want %v", c.name, fp, err, c.want)
		}
		if err != nil && (strings.Contains(err.Error(), "@") || strings.Contains(err.Error(), "someone")) {
			t.Errorf("%s: error names an email: %v", c.name, err)
		}
		if c.want == errAgyOtherAccount && !strings.Contains(err.Error(), "***se") {
			t.Errorf("%s: error does not tell the other account: %v", c.name, err)
		}
	}
	// Magpie lists the account by its email when metadata has none.
	h, home := agyFixture(t, "{}")
	writeFiles(t, h.ConfigDir, map[string]string{"plugin-auth.json": `{"agy-cli":{"type":"api","key":"agy"}}`})
	writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agySignIn("ya29.own", "1//own", testNow.Add(time.Hour), "d@example.com")})
	if fp, err := h.ConfirmIdentity(context.Background(), agyOwn); err != nil || fp != fingerprint("1//own") {
		t.Fatalf("email from logins.json: %q, %v", fp, err)
	}
}

// An account's own home is checked as far as both emails are known.
func TestAgyHomeOtherAccount(t *testing.T) {
	h, _ := agyFixture(t, agySignIn("ya29.live", "1//refresh", testNow.Add(time.Hour), "someone.else@example.org"))
	if fp, err := h.ConfirmIdentity(context.Background(), agyAccount); !errors.Is(err, errAgyOtherAccount) || !errors.Is(err, host.ErrIdentity) || fp != "" || strings.Contains(err.Error(), "@") {
		t.Fatalf("home signed in to another account: %q, %v", fp, err)
	}
	h, _ = agyFixture(t, agySignIn("ya29.live", "1//refresh", testNow.Add(time.Hour), ""))
	if fp, err := h.ConfirmIdentity(context.Background(), agyAccount); err != nil || fp != fingerprint("1//refresh") {
		t.Fatalf("home file without id_token_email: %q, %v", fp, err)
	}
}

// Files and entries of another shape are format errors, and a token file
// whose expiry is not a time does not quote the value: the file is a
// credential.
func TestAgyIdentityFormat(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"agyHome not a home id":          {"plugin-auth.json": `{"agy-cli#k3x9a2":{"type":"api","metadata":{"agyHome":"../../etc"}}}`},
		"plugin-auth.json not an object": {"plugin-auth.json": `[]`},
		"agy-cli login without home":     {"logins.json": `[{"agent":"plugin:agy-cli","user":"c@example.com"}]`, "plugin-auth.json": `{}`},
		"agy-cli entry null":             {"plugin-auth.json": `{"agy-cli#k3x9a2":null}`},
		"agy-cli entry not api":          {"plugin-auth.json": `{"agy-cli#k3x9a2":{"type":"oauth","key":"agy"}}`},
		"agyHome that its key does not name": {
			"plugin-auth.json": `{"agy-cli#k3x9a2":{"type":"api","key":"agy","metadata":{"agyHome":"0123456789abcdef"}}}`,
		},
	} {
		h, _ := agyFixture(t, agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour)))
		writeFiles(t, h.ConfigDir, files)
		if fp, err := h.ConfirmIdentity(context.Background(), agyAccount); !errors.Is(err, host.ErrFormat) || fp != "" {
			t.Errorf("%s: %q, err = %v, want ErrFormat", name, fp, err)
		}
	}
	const value = "ya29.not-a-time"
	for name, token := range map[string]string{
		"token file without token":    `{"token_type":"Bearer"}`,
		"expiry that does not parse":  `{"token":{"access_token":"ya29.live","refresh_token":"1//refresh","expiry":"` + value + `"},"auth_method":"consumer","id_token_email":"c@example.com"}`,
		"token without expiry":        `{"token":{"access_token":"ya29.live","refresh_token":"1//refresh"},"auth_method":"consumer"}`,
		"base64 that does not decode": "go-keyring-base64:%%%",
	} {
		h, _ := agyFixture(t, token)
		fp, err := h.ConfirmIdentity(context.Background(), agyAccount)
		if !errors.Is(err, host.ErrFormat) || fp != "" || strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "ya29") {
			t.Errorf("%s: %q, err = %v, want ErrFormat", name, fp, err)
		}
	}
}

// The default token file holds whichever account agy itself is signed in
// to. An entry that is not known to be agy's own must not read it, or the
// fingerprint of that account would go to this pin.
func TestAgyDefaultFileOnlyForOwnAccount(t *testing.T) {
	defer func(was string) { goos = was }(goos)
	goos = "linux"
	for name, entry := range map[string]string{
		"home key without agyHome": `{"type":"api","key":"agy:0123456789abcdef","metadata":{"email":"c@example.com"}}`,
		"no key":                   `{"type":"api","metadata":{"email":"c@example.com"}}`,
		"no type":                  `{"key":"agy"}`,
		"empty object":             `{}`,
	} {
		h, home := agyFixture(t, "{}")
		writeFiles(t, h.ConfigDir, map[string]string{"plugin-auth.json": `{"agy-cli#k3x9a2":` + entry + `}`})
		writeFiles(t, home, map[string]string{".gemini/antigravity-cli/antigravity-oauth-token": agyToken("ya29.other", "1//other", testNow.Add(time.Hour))})
		if fp, err := h.ConfirmIdentity(context.Background(), agyAccount); !errors.Is(err, host.ErrFormat) || fp != "" {
			t.Errorf("%s: %q, err = %v, want ErrFormat", name, fp, err)
		}
	}
}

// A plugin's account is named by nameID of the provider and the name
// only, never by the id its logins.json entry has.
func TestPluginLoginByNameOnly(t *testing.T) {
	h, _ := agyFixture(t, agyToken("ya29.live", "1//refresh", testNow.Add(time.Hour)))
	_, err := h.ConfirmIdentity(context.Background(), host.Account{Pin: "agy-cli/abcdabcdabcdabcd", Provider: "agy-cli"})
	if err == nil || !strings.Contains(err.Error(), "没有账号") {
		t.Fatalf("pin of the entry's id: err = %v, want no account", err)
	}
}

// The errors of an agy-cli login name no value of the files: the home
// that logins.json gives is the key of plugin-auth.json, any text.
func TestAgyErrorsLeaveOutTheHome(t *testing.T) {
	const home = "carol.secret@corp.example"
	logins := strings.ReplaceAll(loginsFile("", "{}"), "agy-cli#k3x9a2", home)
	for name, auth := range map[string]string{
		"no entry":        `{}`,
		"not an object":   `{"` + home + `":[]}`,
		"not api":         `{"` + home + `":{"type":"oauth","key":"agy"}}`,
		"bad agyHome":     `{"` + home + `":{"type":"api","metadata":{"agyHome":"../../etc"}}}`,
		"key not agyHome": `{"` + home + `":{"type":"api","key":"agy","metadata":{"agyHome":"0123456789abcdef"}}}`,
	} {
		h := &Host{ConfigDir: t.TempDir()}
		writeFiles(t, h.ConfigDir, map[string]string{"logins.json": logins, "plugin-auth.json": auth})
		_, err := h.ConfirmIdentity(context.Background(), agyAccount)
		if err == nil || strings.Contains(err.Error(), "carol") || strings.Contains(err.Error(), "oauth") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
