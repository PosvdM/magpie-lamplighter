package magpie

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strings"
)

// proxyFor is the proxy Magpie sends the requests of account user of
// provider through, as ProxyOfLogin and netproxy.For decide it in Magpie:
//
//  1. the account's own proxy in providers.json (accountProxies, by the
//     account name in lower case), else the provider's (proxy);
//  2. else the global proxy in settings.json (proxy), which Magpie uses
//     for every request to a vendor, whatever NO_PROXY says.
//
// "" means neither is set: the request takes the environment's proxy,
// as egress does for "". Magpie then uses the environment's or the
// system's proxy with its exceptions; it passes that one to the plugin
// host as MAGPIE_HTTPS_PROXY, MAGPIE_HTTP_PROXY and MAGPIE_NO_PROXY, and
// the npm shell starts Lamplighter with them as HTTPS_PROXY, HTTP_PROXY
// and NO_PROXY, so the exceptions apply here too. "direct" means a
// direct connection. A saved proxy ("@<id>") is replaced by its address
// with its login. A saved proxy that is gone or is an SSH server is an
// error: the request must not take another way out.
func (h *Host) proxyFor(provider, user string) (string, error) {
	var file struct {
		Providers []struct {
			ID             string            `json:"id"`
			Proxy          string            `json:"proxy"`
			AccountProxies map[string]string `json:"accountProxies"`
		} `json:"providers"`
	}
	if err := h.readJSONFile("providers.json", &file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	choice := ""
	for _, p := range file.Providers {
		if p.ID != provider {
			continue
		}
		for name, v := range p.AccountProxies {
			if accountKey(name) == accountKey(user) && strings.TrimSpace(v) != "" {
				choice = v
			}
		}
		if choice == "" {
			choice = p.Proxy
		}
		break
	}
	var settings struct {
		Proxy   string       `json:"proxy"`
		Proxies []savedProxy `json:"proxies"`
	}
	if err := h.readJSONFile("settings.json", &settings); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	choice = strings.TrimSpace(choice)
	if choice == "" {
		choice = strings.TrimSpace(settings.Proxy)
	}
	if choice == "" {
		return "", nil
	}
	if strings.EqualFold(choice, "direct") {
		return "direct", nil
	}
	if id, ok := strings.CutPrefix(choice, "@"); ok {
		i := slices.IndexFunc(settings.Proxies, func(p savedProxy) bool {
			return p.ID == id
		})
		if i < 0 {
			return "", fmt.Errorf("Magpie 为 %s 账号设置的代理 %s 已不存在", provider, choice)
		}
		p := settings.Proxies[i]
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.URL)), "ssh://") {
			return "", fmt.Errorf("Magpie 为 %s 账号设置的代理 %q 是 SSH 服务器，Lamplighter 不支持", provider, p.Name)
		}
		u, err := url.Parse(withScheme(strings.TrimSpace(p.URL)))
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("Magpie 的代理 %q 没有可用的地址", p.Name)
		}
		if p.User != "" || p.Pass != "" {
			if p.Pass != "" {
				u.User = url.UserPassword(p.User, p.Pass)
			} else {
				u.User = url.User(p.User)
			}
		}
		return u.String(), nil
	}
	return withScheme(choice), nil
}

// savedProxy is one of the proxies kept in Settings (settings.NamedProxy).
type savedProxy struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

// withScheme reads a bare host:port as an HTTP proxy, as Magpie does.
func withScheme(raw string) string {
	if !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}
