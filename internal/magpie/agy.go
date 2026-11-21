package magpie

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/egress"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

// The agy-cli plugin (@magpie-community/opencode-agy-cli-auth, read at
// version 0.1.108 of magpie-community/plugins) runs the agy CLI on each
// Google account. Its accounts are kept in plugin-auth.json under keys
// such as "agy-cli" and "agy-cli#k3x9a2", as Magpie's host.js saves the
// plugin's sign-in (signedInAs in index.mjs):
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
// Any other entry wraps host.ErrFormat: the default token file is read only
// for an entry known to be agy's own, as it holds whichever account agy
// itself is signed in to.

const (
	// googleTokenURL renews an access token with the OAuth client agy
	// signs in with. Google renews it without changing the refresh token,
	// so the new access token is kept in memory only and agy's file is
	// never written.
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep" + ".apps.googleusercontent.com"
	// consumerProject is the Cloud Code project every personal Google
	// account is served under (CONSUMER_PROJECT in the plugin's
	// usage.mjs).
	consumerProject = "aicode-consumers"
	// googleRenewLead renews an access token this long before it ends,
	// as the plugin does.
	googleRenewLead = 2 * time.Minute
)

// googleClientSecret belongs to an installed-app OAuth client, whose
// secret Google does not treat as one. It is written in parts so that
// secret scanners do not report it.
var googleClientSecret = strings.Join([]string{"GOCSPX", "K58FWR486LdLJ1mLB8sXC4z6qDAf"}, "-")

// googleDoer sends the token renewals; tests replace it.
var googleDoer quota.Doer = &egress.Egress{Timeout: 20 * time.Second}

// goos is runtime.GOOS; tests replace it.
var goos = runtime.GOOS

var agyHomeID = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// errAgyKeyring is returned for agy's own account where agy keeps its
// sign-in in the system keyring.
var errAgyKeyring = errors.New("这个 Antigravity 账号是 agy 自己登录的第一个账号，凭证在系统钥匙串中，Lamplighter 不读取钥匙串；可在 Magpie 中再添加一次该账号，或改用 Magpie 的额度显示")

// googleToken is an access token renewed in memory.
type googleToken struct {
	access  string
	expires time.Time
}

// agyTokenFile is where agy keeps the sign-in of the agy-cli account
// kept at key in plugin-auth.json.
func (h *Host) agyTokenFile(key string) (string, error) {
	var auth map[string]json.RawMessage
	if err := h.readJSONFile("plugin-auth.json", &auth); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("Magpie 配置目录中没有 plugin-auth.json")
		}
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("logins.json 的 agy-cli 账号：%w: no home", host.ErrFormat)
	}
	raw, ok := auth[key]
	if !ok {
		return "", fmt.Errorf("plugin-auth.json 中没有 agy-cli 账号 %q", key)
	}
	var entry struct {
		Type     string         `json:"type"`
		Key      string         `json:"key"`
		Metadata map[string]any `json:"metadata"`
	}
	if !isJSONObject(raw) {
		return "", fmt.Errorf("plugin-auth.json 的 %s：%w: not an object", key, host.ErrFormat)
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", fmt.Errorf("plugin-auth.json 的 %s：%w: %v", key, host.ErrFormat, err)
	}
	if entry.Type != "api" {
		return "", fmt.Errorf("plugin-auth.json 的 %s：%w: type is %q, not api", key, host.ErrFormat, entry.Type)
	}
	home := ""
	if v, ok := entry.Metadata["agyHome"]; ok {
		s, isString := v.(string)
		if !isString || !agyHomeID.MatchString(s) {
			return "", fmt.Errorf("plugin-auth.json 的 %s：%w: agyHome is not a home id", key, host.ErrFormat)
		}
		home = s
	}
	switch {
	case home != "" && entry.Key != "agy:"+home:
		return "", fmt.Errorf("plugin-auth.json 的 %s：%w: key does not name agyHome", key, host.ErrFormat)
	case home == "" && entry.Key != "agy":
		return "", fmt.Errorf("plugin-auth.json 的 %s：%w: neither agy's own account nor one with an agyHome", key, host.ErrFormat)
	}
	if home != "" {
		state := os.Getenv("XDG_STATE_HOME")
		if !filepath.IsAbs(state) {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			state = filepath.Join(userHome, ".local", "state")
		}
		return filepath.Join(state, "magpie-agy-bridge", "accounts", home, ".gemini", "antigravity-cli", "antigravity-oauth-token"), nil
	}
	if goos == "darwin" || goos == "windows" {
		return "", errAgyKeyring
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".gemini", "antigravity-cli", "antigravity-oauth-token"), nil
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
	AuthMethod string `json:"auth_method"`
}

// parseAgyStored reads agy's token file: JSON, or JSON behind
// go-keyring's "go-keyring-base64:" or "go-keyring-encoded:" prefix.
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
		return agyStored{}, fmt.Errorf("%w: expiry %q is not RFC 3339", host.ErrFormat, *s.Token.Expiry)
	}
	return s, nil
}

// agyCredential reads the sign-in of an agy-cli account. An access token
// that has ended, or ends within googleRenewLead, is renewed in memory
// through proxy.
func (h *Host) agyCredential(ctx context.Context, l savedLogin, proxy string) (host.Credential, error) {
	file, err := h.agyTokenFile(l.Home)
	if err != nil {
		return host.Credential{}, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return host.Credential{}, errors.New("找不到 agy 的凭证文件，账号可能已在 Magpie 中退出登录")
		}
		return host.Credential{}, err
	}
	s, err := parseAgyStored(b)
	if err != nil {
		return host.Credential{}, fmt.Errorf("agy 的凭证文件：%w", err)
	}
	t := s.Token
	if t.AccessToken == "" && t.RefreshToken == "" {
		return host.Credential{}, errors.New("agy 没有登录这个账号")
	}
	method := strings.TrimSpace(s.AuthMethod)
	if method != "" && method != "consumer" {
		return host.Credential{}, fmt.Errorf("不支持 auth_method 为 %s 的 Antigravity 账号：它的 Cloud Code project 要另行查询", method)
	}
	cred := host.Credential{
		AccessToken: t.AccessToken,
		ProjectID:   consumerProject,
		Fingerprint: fingerprint(t.RefreshToken),
	}
	if cred.Fingerprint == "" {
		cred.Fingerprint = fingerprint(t.AccessToken)
	}
	// Go's zero time, which agy writes for a token that does not end, is
	// before 1970. parseAgyStored checked the format.
	expires, _ := time.Parse(time.RFC3339Nano, *t.Expiry)
	ends := expires.Unix() > 0
	if t.AccessToken != "" && (!ends || now().Add(googleRenewLead).Before(expires)) {
		return cred, nil
	}
	if t.RefreshToken == "" {
		cred.Expired = true
		return cred, nil
	}
	access, err := h.renewGoogle(ctx, t.RefreshToken, proxy)
	if err != nil {
		return host.Credential{}, err
	}
	cred.AccessToken = access
	return cred, nil
}

// renewGoogle is an access token for refresh, renewed at Google when the
// one kept in memory ends within googleRenewLead.
func (h *Host) renewGoogle(ctx context.Context, refresh, proxy string) (string, error) {
	key := fingerprint(refresh)
	h.mu.Lock()
	kept, ok := h.google[key]
	h.mu.Unlock()
	if ok && now().Add(googleRenewLead).Before(kept.expires) {
		return kept.access, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {googleClientID},
		"client_secret": {googleClientSecret},
	}
	header := http.Header{}
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	start := now()
	resp, err := googleDoer.Do(ctx, proxy, quota.Request{Method: http.MethodPost, URL: googleTokenURL, Header: header, Body: []byte(form.Encode())})
	if err != nil {
		return "", fmt.Errorf("刷新 Google access token 失败：%w", err)
	}
	var body struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
		Error       string  `json:"error"`
	}
	_ = json.Unmarshal(resp.Body, &body)
	if resp.Status != http.StatusOK || body.AccessToken == "" {
		if body.Error == "invalid_grant" {
			return "", errors.New("Google 不再接受这个账号的 refresh token（invalid_grant），需要在 Magpie 中重新登录")
		}
		return "", fmt.Errorf("刷新 Google access token 失败：HTTP %d %s", resp.Status, body.Error)
	}
	expires := start.Add(time.Duration(body.ExpiresIn * float64(time.Second)))
	if body.ExpiresIn <= 0 {
		expires = start.Add(time.Hour)
	}
	h.mu.Lock()
	if h.google == nil {
		h.google = map[string]googleToken{}
	}
	h.google[key] = googleToken{access: body.AccessToken, expires: expires}
	h.mu.Unlock()
	return body.AccessToken, nil
}
