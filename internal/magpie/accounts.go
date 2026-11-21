package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// The gateway answers this computer without a key: a request that reaches
// it on loopback, carries no forwarding header (X-Forwarded-For, Forwarded,
// X-Real-IP, Cf-Connecting-IP and the like) and does not look like a
// browser (Origin, Sec-Fetch-*, a Mozilla User-Agent) is local to Magpie
// (lanGuard and local in Magpie's internal/gateway/lan.go). The requests
// here send none of those and never go through a proxy.

// accountsPath lists every account and key (internal/gateway/accounts_list.go).
const accountsPath = "/v1/magpie/accounts"

// maxGatewayBody bounds a gateway response read here.
const maxGatewayBody = 16 << 20

// gatewayClient talks to the gateway directly, never through the
// environment's proxy.
var gatewayClient = &http.Client{Transport: gatewayTransport}

// now is the clock of this package; tests replace it.
var now = time.Now

// hostState is what a Host keeps between calls: in-memory Google tokens
// and the last quota refresh of each account. magpie.go owns the Host
// struct, so the state is kept beside it, one per Host.
type hostState struct {
	mu        sync.Mutex
	google    map[string]googleToken
	refreshed map[string]time.Time
}

var states sync.Map // *Host → *hostState

func (h *Host) state() *hostState {
	v, _ := states.LoadOrStore(h, &hostState{google: map[string]googleToken{}, refreshed: map[string]time.Time{}})
	return v.(*hostState)
}

// gatewayURL is path on the gateway at h.Addr. An address on every
// interface (":3425", "0.0.0.0:3425") is reached on loopback, as Magpie's
// own plugin host does.
func (h *Host) gatewayURL(path string) (string, error) {
	addr := strings.TrimSpace(h.Addr)
	if addr == "" {
		return "", errors.New("MAGPIE_ADDR 未设置，找不到 Magpie 网关")
	}
	if strings.Contains(addr, "://") {
		return strings.TrimRight(addr, "/") + path, nil
	}
	if hostPart, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(hostPart); hostPart == "" || ip != nil && ip.IsUnspecified() {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return "http://" + addr + path, nil
}

// gatewayJSON sends one request to the gateway and decodes its JSON answer
// into out. A status other than 2xx is a *host.Error; an answer that is not
// JSON of out's shape wraps host.ErrFormat.
func (h *Host) gatewayJSON(ctx context.Context, method, path string, out any) error {
	url, err := h.gatewayURL(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "magpie-lamplighter")
	resp, err := gatewayClient.Do(req)
	if err != nil {
		return &host.Error{Method: method + " " + routeOf(path), Code: "gateway_unreachable", Message: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &host.Error{Method: method + " " + routeOf(path), Code: "gateway_error", Message: gatewayMessage(body), Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: %w: %v", method, routeOf(path), host.ErrFormat, err)
	}
	return nil
}

func routeOf(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

// gatewayMessage is the message of a gateway error body: Magpie answers
// {"error": {"message": …}}, else the start of the text.
func gatewayMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	text := strings.TrimSpace(string(bytes.ToValidUTF8(body, nil)))
	if len(text) > 300 {
		text = text[:300]
	}
	if text == "" {
		text = "empty response"
	}
	return text
}

// listedAccount is one entry of GET /v1/magpie/accounts.
type listedAccount struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Pin      string `json:"pin"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	User     string `json:"user"`
	Plan     string `json:"plan"`
	On       *bool  `json:"on"`
}

// listAccounts reads the gateway's account list, checking its shape.
func (h *Host) listAccounts(ctx context.Context) ([]listedAccount, error) {
	var resp struct {
		Data *[]listedAccount `json:"data"`
	}
	if err := h.gatewayJSON(ctx, http.MethodGet, accountsPath, &resp); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("GET %s: %w: no data list", accountsPath, host.ErrFormat)
	}
	for i, a := range *resp.Data {
		if a.Provider == "" || a.ID == "" || a.Pin != a.Provider+"/"+a.ID || a.On == nil || a.Kind != "account" && a.Kind != "key" {
			return nil, fmt.Errorf("GET %s: %w: entry %d is not {provider, id, pin, kind, on}", accountsPath, host.ErrFormat, i)
		}
	}
	return *resp.Data, nil
}

// Accounts lists the signed-in subscription accounts. API keys, also on
// the list, have no quota windows and are left out.
func (h *Host) Accounts(ctx context.Context) ([]host.Account, error) {
	listed, err := h.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := []host.Account{}
	for _, a := range listed {
		if a.Kind != "account" {
			continue
		}
		out = append(out, host.Account{
			Pin:      a.Pin,
			Provider: a.Provider,
			Label:    strings.TrimSpace(a.Label),
			Email:    emailOf(a.User),
			Plan:     a.Plan,
			On:       *a.On,
		})
	}
	return out, nil
}

// emailOf is the email in a Magpie account name: the name itself, or the
// part before " · " of a workspace seat such as "a@b.com · Team". "" when
// the name is not an email.
func emailOf(user string) string {
	user, _, _ = strings.Cut(strings.TrimSpace(user), " · ")
	user = strings.TrimSpace(user)
	if !strings.Contains(user, "@") {
		return ""
	}
	return user
}

// accountByPin is the listed account with pin.
func (h *Host) accountByPin(ctx context.Context, pin string) (listedAccount, error) {
	listed, err := h.listAccounts(ctx)
	if err != nil {
		return listedAccount{}, err
	}
	for _, a := range listed {
		if strings.EqualFold(a.Pin, pin) {
			return a, nil
		}
	}
	return listedAccount{}, fmt.Errorf("Magpie 没有账号 %s", pin)
}
