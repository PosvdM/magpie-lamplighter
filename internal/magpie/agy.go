package magpie

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// The agy-cli plugin (@magpie-community/opencode-agy-cli-auth, read at
// version 0.1.151 of magpie-community/plugins) runs the agy CLI on each
// Google account. Magpie lists each account under logins.json with agent
// "plugin:agy-cli" and the account's key in plugin-auth.json as home, such
// as "agy-cli" or "agy-cli#k3x9a2"; host.js saves the plugin's sign-in
// there (signedInAs in index.mjs):
//
//   - {"type": "api", "key": "agy:<home>", "metadata": {"email", "agyHome": "<home>"}}
//     is an account with a home of its own, whose agy keeps its sign-in in
//     a file under $XDG_STATE_HOME/magpie-agy-bridge/accounts/<home>
//     (account.mjs);
//   - {"type": "api", "key": "agy", "metadata": {"email"}} is agy's own
//     account: agy keeps it in the system keyring on macOS and Windows, and
//     in ~/.gemini/antigravity-cli on Linux without a keyring, such as
//     Magpie's Docker image. Lamplighter does not read a keyring, which can
//     ask the user for permission.
//
// The gateway sends an agy-cli account's requests through agy, with
// whichever Google account agy is signed in to. Before ignition, the
// engine has the sign-in confirmed (ConfirmIdentity) from these local
// files alone: nothing here sends a request, and the tokens read serve
// only to tell one sign-in from another (agyFingerprint). They leave this
// file only as that fingerprint.
//
// Any other entry wraps host.ErrFormat: the default token file is read only
// for an entry known to be agy's own, as it holds whichever account agy
// itself is signed in to. Its sign-in is confirmed only when the file's
// id_token_email, which agy writes at sign-in (emailOf in the plugin's
// signin.mjs), is the entry's email. A home's file belongs to its account
// alone; it is refused only when both emails are known and differ.
//
// The refusals, and agy's own account in a keyring, wrap host.ErrIdentity,
// and the engine does not ignite the account. The entry's email is written
// only by a sign-in through Magpie; signing in to agy again does not add
// it.

// goos is runtime.GOOS; tests replace it.
var goos = runtime.GOOS

var agyHomeID = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// identityError is an error that wraps host.ErrIdentity: the sign-in read
// for an agy-cli account cannot be told to be that account's.
type identityError string

func (e identityError) Error() string { return string(e) }

func (identityError) Is(target error) bool { return target == host.ErrIdentity }

// errAgyKeyring is returned for agy's own account where agy keeps its
// sign-in in the system keyring. Without reading it, the account agy is
// signed in to now cannot be confirmed. The plugin's "添加另一个 Google
// 账号" signs in in a home of its own, whose file Lamplighter reads.
var errAgyKeyring error = identityError("这个 Antigravity 账号是 agy 自己登录的账号，凭证在系统钥匙串中，Lamplighter 不读取钥匙串，无法确认 agy 当前登录的账号；在 Magpie 中用“添加另一个 Google 账号”重新添加该账号后即可点火")

// errAgyOtherAccount is returned when agy's token file is signed in to
// another Google account than the agy-cli account.
var errAgyOtherAccount error = identityError("agy 的凭证属于另一个 Google 账号")

// errAgyUnproven is returned when it cannot be told whether agy's default
// token file is signed in to agy's own account.
var errAgyUnproven error = identityError("无法确认 agy 当前登录的是这个 Google 账号")

// notFound is a file that does not exist, with a text of its own; it
// wraps fs.ErrNotExist.
type notFound string

func (e notFound) Error() string { return string(e) }

func (notFound) Is(target error) bool { return target == fs.ErrNotExist }

// agyFile is where agy keeps the sign-in of an agy-cli account.
type agyFile struct {
	path string
	// own is agy's own account: path is agy's default file, which holds
	// whichever account agy is signed in to now.
	own bool
	// email is the account's Google email as Magpie keeps it, "" when
	// Magpie has none.
	email string
}

// agyTokenFile is where agy keeps the sign-in of the agy-cli account l,
// kept at l.Home in plugin-auth.json.
func (h *Host) agyTokenFile(l savedLogin) (agyFile, error) {
	var auth map[string]json.RawMessage
	if err := h.readJSONFile("plugin-auth.json", &auth); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return agyFile{}, notFound("Magpie 配置目录中没有 plugin-auth.json")
		}
		return agyFile{}, err
	}
	key := l.Home
	if key == "" {
		return agyFile{}, fmt.Errorf("logins.json 的 agy-cli 账号：%w: no home", host.ErrFormat)
	}
	raw, ok := auth[key]
	if !ok {
		return agyFile{}, errors.New("plugin-auth.json 中没有这个 agy-cli 账号")
	}
	var entry struct {
		Type     string         `json:"type"`
		Key      string         `json:"key"`
		Metadata map[string]any `json:"metadata"`
	}
	if !isJSONObject(raw) {
		return agyFile{}, fmt.Errorf("plugin-auth.json 中这个 agy-cli 账号的条目：%w: not an object", host.ErrFormat)
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return agyFile{}, fmt.Errorf("plugin-auth.json 中这个 agy-cli 账号的条目：%w: %v", host.ErrFormat, err)
	}
	if entry.Type != "api" {
		return agyFile{}, fmt.Errorf("plugin-auth.json 中这个 agy-cli 账号的条目：%w: type is not api", host.ErrFormat)
	}
	home := ""
	if v, ok := entry.Metadata["agyHome"]; ok {
		s, isString := v.(string)
		if !isString || !agyHomeID.MatchString(s) {
			return agyFile{}, fmt.Errorf("plugin-auth.json 中这个 agy-cli 账号的条目：%w: agyHome is not a home id", host.ErrFormat)
		}
		home = s
	}
	switch {
	case home != "" && entry.Key != "agy:"+home:
		return agyFile{}, fmt.Errorf("plugin-auth.json 中这个 agy-cli 账号的条目：%w: key does not name agyHome", host.ErrFormat)
	case home == "" && entry.Key != "agy":
		return agyFile{}, fmt.Errorf("plugin-auth.json 中这个 agy-cli 账号的条目：%w: neither agy's own account nor one with an agyHome", host.ErrFormat)
	}
	// The plugin names the account by its email in metadata, and Magpie
	// lists it by that name.
	email, _ := entry.Metadata["email"].(string)
	email = strings.TrimSpace(email)
	if email == "" {
		email = emailOf(l.User)
	}
	if home != "" {
		state := os.Getenv("XDG_STATE_HOME")
		if !filepath.IsAbs(state) {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return agyFile{}, err
			}
			state = filepath.Join(userHome, ".local", "state")
		}
		return agyFile{path: filepath.Join(state, "magpie-agy-bridge", "accounts", home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), email: email}, nil
	}
	if goos == "darwin" || goos == "windows" {
		return agyFile{}, errAgyKeyring
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return agyFile{}, err
	}
	return agyFile{path: filepath.Join(userHome, ".gemini", "antigravity-cli", "antigravity-oauth-token"), own: true, email: email}, nil
}

// belongs checks that the sign-in in f, whose id_token_email is signedIn,
// is f's account. agy's default file must name the account's email; a
// home's file is refused only when it names another one. Emails are told
// apart case-insensitively, and an error shows only the last two
// characters of the other account's name.
func (f agyFile) belongs(signedIn string) error {
	signedIn = strings.TrimSpace(signedIn)
	if f.email != "" && signedIn != "" && !strings.EqualFold(f.email, signedIn) {
		if f.own {
			return fmt.Errorf("%w（%s）：agy 已改登其他账号，在 agy 中改回这个账号登录，或在 Magpie 中用“添加另一个 Google 账号”重新添加这个账号", errAgyOtherAccount, maskEmail(signedIn))
		}
		return fmt.Errorf("%w（%s）：需要在 Magpie 中重新登录这个账号", errAgyOtherAccount, maskEmail(signedIn))
	}
	if !f.own {
		return nil
	}
	switch {
	case f.email == "":
		return fmt.Errorf("%w：Magpie 没有记下这个账号的邮箱，在 agy 中重新登录不能补上，需在 Magpie 中用“添加另一个 Google 账号”重新添加这个账号", errAgyUnproven)
	case signedIn == "":
		return fmt.Errorf("%w：agy 的凭证文件没有记下所属账号（id_token_email），需在 Magpie 中用“添加另一个 Google 账号”重新添加这个账号", errAgyUnproven)
	}
	return nil
}

// agyStored is agy's stored sign-in, as go-keyring writes it.
type agyStored struct {
	Token *struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		// Expiry is RFC 3339, as Go writes a time.Time; Go's zero time
		// is a token that does not end.
		Expiry *string `json:"expiry"`
	} `json:"token"`
	// IDTokenEmail is the Google account agy signed in to, which agy
	// takes from the sign-in's ID token.
	IDTokenEmail string `json:"id_token_email"`
}

// parseAgyStored reads agy's token file: JSON, or JSON behind
// go-keyring's "go-keyring-base64:" or "go-keyring-encoded:" prefix. A
// file of another shape wraps host.ErrFormat and quotes no value.
func parseAgyStored(b []byte) (agyStored, error) {
	text := strings.TrimSpace(string(b))
	switch {
	case strings.HasPrefix(text, "go-keyring-base64:"):
		d, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(text, "go-keyring-base64:"))
		if err != nil {
			return agyStored{}, fmt.Errorf("%w: %v", host.ErrFormat, err)
		}
		text = string(d)
	case strings.HasPrefix(text, "go-keyring-encoded:"):
		d, err := hex.DecodeString(strings.TrimPrefix(text, "go-keyring-encoded:"))
		if err != nil {
			return agyStored{}, fmt.Errorf("%w: %v", host.ErrFormat, err)
		}
		text = string(d)
	}
	var s agyStored
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		return agyStored{}, fmt.Errorf("%w: %v", host.ErrFormat, err)
	}
	if s.Token == nil {
		return agyStored{}, fmt.Errorf("%w: no token", host.ErrFormat)
	}
	if s.Token.Expiry == nil {
		return agyStored{}, fmt.Errorf("%w: token without expiry", host.ErrFormat)
	}
	if _, err := time.Parse(time.RFC3339Nano, *s.Token.Expiry); err != nil {
		// The value is left out: the file is a credential.
		return agyStored{}, fmt.Errorf("%w: token expiry is not RFC 3339", host.ErrFormat)
	}
	return s, nil
}

// agySignIn reads agy's sign-in of the agy-cli account l from local files
// alone, and refuses one that is not l's (agyFile.belongs).
func (h *Host) agySignIn(l savedLogin) (agyStored, error) {
	file, err := h.agyTokenFile(l)
	if err != nil {
		return agyStored{}, err
	}
	b, err := os.ReadFile(file.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return agyStored{}, notFound("找不到 agy 的凭证文件，账号可能已在 Magpie 中退出登录")
		}
		return agyStored{}, err
	}
	s, err := parseAgyStored(b)
	if err != nil {
		return agyStored{}, fmt.Errorf("agy 的凭证文件：%w", err)
	}
	if s.Token.AccessToken == "" && s.Token.RefreshToken == "" {
		return agyStored{}, errors.New("agy 没有登录这个账号")
	}
	if err := file.belongs(s.IDTokenEmail); err != nil {
		return agyStored{}, err
	}
	return s, nil
}

// agyFingerprint is the fingerprint of the sign-in s: of its refresh
// token, else of its access token.
func agyFingerprint(s agyStored) string {
	if fp := fingerprint(s.Token.RefreshToken); fp != "" {
		return fp
	}
	return fingerprint(s.Token.AccessToken)
}

// ConfirmIdentity implements host.IdentityChecker. The gateway sends an
// agy-cli account's requests with whichever Google account its agy is
// signed in to; the sign-in is read from local files alone and must be
// the account's (agyFile.belongs). Codex and Claude requests use the
// account's own credential, which needs no check: they return "" and nil.
// Any error means the account is not confirmed.
func (h *Host) ConfirmIdentity(_ context.Context, account host.Account) (string, error) {
	provider, l, err := h.accountLogin(account)
	if err != nil {
		return "", err
	}
	if provider != agyCLI {
		return "", nil
	}
	s, err := h.agySignIn(l)
	if err != nil {
		return "", err
	}
	return agyFingerprint(s), nil
}
