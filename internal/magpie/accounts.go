package magpie

import (
	"context"
	"fmt"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

func (h *Host) Accounts(context.Context) ([]host.Account, error) {
	return nil, fmt.Errorf("accounts: %w", host.ErrNotImplemented)
}

func (h *Host) Credential(context.Context, host.Account) (host.Credential, error) {
	return host.Credential{}, fmt.Errorf("credential: %w", host.ErrNotImplemented)
}

func (h *Host) KeptQuotas(context.Context) ([]host.KeptQuota, error) {
	return nil, fmt.Errorf("kept quotas: %w", host.ErrNotImplemented)
}

func (h *Host) RefreshQuota(context.Context, string) error {
	return fmt.Errorf("refresh quota: %w", host.ErrNotImplemented)
}
