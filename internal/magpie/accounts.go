package magpie

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// accountsPath lists every account and key (internal/gateway/accounts_list.go).
const accountsPath = "/v1/magpie/accounts"

// now is the clock of this package; tests replace it.
var now = time.Now

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
			User:     strings.TrimSpace(a.User),
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
