package magpie

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// savedLogin is one entry of logins.json (savedLogin in Magpie's
// internal/provider/logins.go). Auth is the agent's own credential blob:
// Codex's auth.json, Claude Code's .credentials.json. A plugin's account
// has agent "plugin:<id>" and no Auth; Home is its key in
// plugin-auth.json.
type savedLogin struct {
	ID    string          `json:"id"`
	Agent string          `json:"agent"`
	User  string          `json:"user"`
	Home  string          `json:"home"`
	Auth  json.RawMessage `json:"auth"`
}

// accountKey is how Magpie tells account names apart.
func accountKey(user string) string { return strings.ToLower(strings.TrimSpace(user)) }

// nameID is the id Magpie gives an account whose logins.json entry has no
// id of its own, and every plugin account (LoginID, loginID).
func nameID(agent, user string) string {
	sum := sha256.Sum256([]byte("magpie login\n" + agent + "\n" + accountKey(user)))
	return hex.EncodeToString(sum[:8])
}

// fingerprint is the first 8 bytes of the SHA-256 of secret, hex.
func fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}

// readJSONFile decodes the file name in Magpie's configuration directory
// into out. A file that does not exist gives an error matching
// fs.ErrNotExist; one that does not decode wraps host.ErrFormat.
func (h *Host) readJSONFile(name string, out any) error {
	if h.ConfigDir == "" {
		return errors.New("找不到 Magpie 的配置目录")
	}
	b, err := os.ReadFile(filepath.Join(h.ConfigDir, name))
	if err != nil {
		return err
	}
	b = []byte(strings.TrimPrefix(string(b), "\ufeff"))
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: %w: %v", name, host.ErrFormat, err)
	}
	return nil
}

// login is the logins.json entry of the account pin of provider.
// Magpie names a Codex or Claude account by its entry's id, else by
// nameID of the agent and name, and a plugin's account by nameID of the
// plugin's provider id and the name (AccountIDs in
// internal/provider/account_models.go).
func (h *Host) login(provider, id string) (savedLogin, error) {
	var ls []savedLogin
	if err := h.readJSONFile("logins.json", &ls); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return savedLogin{}, errors.New("Magpie 配置目录中没有 logins.json")
		}
		return savedLogin{}, err
	}
	for _, l := range ls {
		switch {
		case l.Agent == provider:
			own := l.ID
			if own == "" {
				own = nameID(l.Agent, l.User)
			}
			if strings.EqualFold(own, id) {
				return l, nil
			}
		case l.Agent == "plugin:"+provider:
			if strings.EqualFold(nameID(provider, l.User), id) || l.ID != "" && strings.EqualFold(l.ID, id) {
				return l, nil
			}
		}
	}
	return savedLogin{}, fmt.Errorf("logins.json 中没有账号 %s/%s", provider, id)
}

// agyCLI is the provider id of the agy-cli plugin
// (@magpie-community/opencode-agy-cli-auth), which serves Antigravity.
const agyCLI = "agy-cli"

// Credential reads the credential of account from Magpie's files: Codex
// and Claude from logins.json, Antigravity from the agy-cli plugin's
// token file. Codex and Claude tokens are never refreshed here; one past
// its expiry is returned with Expired set.
func (h *Host) Credential(ctx context.Context, account host.Account) (host.Credential, error) {
	pinProvider, id, ok := strings.Cut(strings.TrimSpace(account.Pin), "/")
	if !ok || pinProvider == "" || id == "" {
		return host.Credential{}, fmt.Errorf("账号 pin %q 不是 <服务>/<id>", account.Pin)
	}
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	if provider == "" {
		provider = strings.ToLower(pinProvider)
	}
	l, err := h.login(provider, id)
	if err != nil {
		return host.Credential{}, err
	}
	var cred host.Credential
	switch provider {
	case "codex":
		cred, err = codexCredential(l.Auth)
	case "claude":
		cred, err = h.claudeCredential(l)
	case agyCLI:
		var proxy string
		if proxy, err = h.proxyFor(provider, l.User); err != nil {
			return host.Credential{}, err
		}
		cred, err = h.agyCredential(ctx, l, proxy)
		cred.ProxyURL = proxy
	default:
		return host.Credential{}, fmt.Errorf("不支持读取 %s 账号的凭证", provider)
	}
	if err != nil {
		return host.Credential{}, err
	}
	if provider != agyCLI {
		if cred.ProxyURL, err = h.proxyFor(provider, l.User); err != nil {
			return host.Credential{}, err
		}
	}
	cred.Provider, cred.ID, cred.Name = provider, account.Pin, account.Pin
	return cred, nil
}

// codexCredential reads a Codex auth.json kept in logins.json.
func codexCredential(auth json.RawMessage) (host.Credential, error) {
	var a struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(auth, &a); err != nil {
		return host.Credential{}, fmt.Errorf("logins.json 的 Codex 凭证：%w: %v", host.ErrFormat, err)
	}
	if strings.EqualFold(a.AuthMode, "apikey") {
		return host.Credential{}, errors.New("Codex 账号用 API key 登录，没有订阅额度")
	}
	if a.Tokens == nil {
		return host.Credential{}, fmt.Errorf("logins.json 的 Codex 凭证：%w: no tokens", host.ErrFormat)
	}
	t := a.Tokens
	cred := host.Credential{AccessToken: t.AccessToken, AccountID: t.AccountID}
	if cred.AccountID == "" {
		if claims, _ := jwtClaims(t.IDToken)["https://api.openai.com/auth"].(map[string]any); claims != nil {
			cred.AccountID, _ = claims["chatgpt_account_id"].(string)
		}
	}
	cred.Fingerprint = fingerprint(t.RefreshToken)
	if cred.Fingerprint == "" {
		cred.Fingerprint = fingerprint(t.AccessToken)
	}
	if exp, ok := jwtClaims(t.AccessToken)["exp"].(float64); ok && exp > 0 && !now().Before(time.Unix(int64(exp), 0)) {
		cred.Expired = true
	}
	return cred, nil
}

// jwtClaims are the claims of a JWT, nil when it is not one.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

// claudeOAuth is Claude Code's claudeAiOauth.
type claudeOAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// ExpiresAt is in milliseconds since the Unix epoch.
	ExpiresAt int64 `json:"expiresAt"`
}

func parseClaudeAuth(b []byte) (oauth *claudeOAuth, setupToken string, err error) {
	var a struct {
		OAuth      *claudeOAuth `json:"claudeAiOauth"`
		SetupToken string       `json:"setupToken"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, "", fmt.Errorf("%w: %v", host.ErrFormat, err)
	}
	if a.OAuth == nil && a.SetupToken == "" {
		return nil, "", fmt.Errorf("%w: neither claudeAiOauth nor setupToken", host.ErrFormat)
	}
	return a.OAuth, a.SetupToken, nil
}

// claudeCredential reads a Claude account: its copy in logins.json, or
// the sign-in Claude Code keeps in the account's own config directory
// under claude-accounts when that one lasts longer. Magpie copies the
// latter back into logins.json only from time to time.
//
// An account added with a setup-token has no token the usage API takes;
// it is returned as Expired, so the engine reads Magpie's kept quota.
func (h *Host) claudeCredential(l savedLogin) (host.Credential, error) {
	oauth, setup, err := parseClaudeAuth(l.Auth)
	if err != nil {
		return host.Credential{}, fmt.Errorf("logins.json 的 Claude 凭证：%w", err)
	}
	if oauth == nil {
		return host.Credential{Fingerprint: fingerprint(setup), Expired: true}, nil
	}
	sum := sha256.Sum256([]byte(accountKey(l.User)))
	file := filepath.Join(h.ConfigDir, "claude-accounts", hex.EncodeToString(sum[:])[:16], ".credentials.json")
	if b, err := os.ReadFile(file); err == nil {
		if own, _, err := parseClaudeAuth(b); err == nil && own != nil && own.AccessToken != "" && own.ExpiresAt > oauth.ExpiresAt {
			oauth = own
		}
	}
	cred := host.Credential{AccessToken: oauth.AccessToken, Fingerprint: fingerprint(oauth.RefreshToken)}
	if cred.Fingerprint == "" {
		cred.Fingerprint = fingerprint(oauth.AccessToken)
	}
	if oauth.ExpiresAt > 0 && !now().Before(time.UnixMilli(oauth.ExpiresAt)) {
		cred.Expired = true
	}
	return cred, nil
}
