package magpie

import (
	"context"
	"fmt"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

func (h *Host) Models(context.Context) ([]string, error) {
	return nil, fmt.Errorf("models: %w", host.ErrNotImplemented)
}

func (h *Host) OpenModelStream(context.Context, host.ModelRequest) (host.ModelStream, error) {
	return host.ModelStream{}, fmt.Errorf("model stream: %w", host.ErrNotImplemented)
}

func (h *Host) ReadModelStream(string) (host.StreamChunk, error) {
	return host.StreamChunk{}, fmt.Errorf("model stream: %w", host.ErrNotImplemented)
}

func (h *Host) CloseModelStream(string) error { return nil }
