package magpie

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// accountHeader pins a request to one account (Magpie's AccountHeader).
const accountHeader = "X-Magpie-Account"

// readChunkSize is the most one ReadModelStream returns.
const readChunkSize = 32 << 10

// modelsPath lists the models of every account.
const modelsPath = "/v1/models"

// Models returns the model ids of GET /v1/models. Magpie names a model
// "<provider>/<model>"; an id without a provider, which Magpie lists when
// the user asks for bare ids, gets its owned_by as the provider, so that
// every id names the provider that serves it. The call ends within
// listTimeout.
func (h *Host) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var body json.RawMessage
	if err := h.gatewayJSON(ctx, http.MethodGet, modelsPath, &body); err != nil {
		return nil, err
	}
	return parseModels(body)
}

func parseModels(body []byte) ([]string, error) {
	var list struct {
		Data *[]struct {
			ID      *string `json:"id"`
			OwnedBy string  `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Data == nil {
		return nil, fmt.Errorf("models: %w: no data list", host.ErrFormat)
	}
	out := make([]string, 0, len(*list.Data))
	seen := map[string]bool{}
	for _, item := range *list.Data {
		if item.ID == nil {
			return nil, fmt.Errorf("models: %w: a model without an id", host.ErrFormat)
		}
		id := strings.TrimSpace(*item.ID)
		if id == "" {
			continue
		}
		if !strings.Contains(id, "/") && strings.TrimSpace(item.OwnedBy) != "" {
			id = strings.TrimSpace(item.OwnedBy) + "/" + id
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// modelPaths maps a ModelRequest protocol to its gateway path.
var modelPaths = map[string]string{
	"openai-response": "/v1/responses",
	"claude":          "/v1/messages",
	"openai":          "/v1/chat/completions",
}

// stream is one open model response.
type stream struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	// mu serializes reads; Close does not take it, so it can cancel a
	// blocked read.
	mu sync.Mutex
}

// streams holds the open streams of a Host by their random ids.
type streams struct {
	mu sync.Mutex
	m  map[string]*stream
}

func (s *streams) add(st *stream) string {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*stream{}
	}
	s.m[id] = st
	return id
}

func (s *streams) get(id string) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id]
}

func (s *streams) remove(id string) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[id]
	delete(s.m, id)
	return st
}

// pinValue is the X-Magpie-Account value of pin. A header value is ASCII,
// so Magpie takes a percent-encoded value for other letters and decodes it.
func pinValue(pin string) string {
	for i := 0; i < len(pin); i++ {
		if pin[i] >= 0x80 || pin[i] < 0x20 {
			return url.PathEscape(pin)
		}
	}
	return pin
}

// OpenModelStream sends one ignition request and returns once the response
// headers arrive. The request lives until CloseModelStream or until ctx is
// cancelled. An error status returns the stream's status and headers with
// a host.Error built from the body, and no stream to read.
func (h *Host) OpenModelStream(ctx context.Context, mr host.ModelRequest) (host.ModelStream, error) {
	path, ok := modelPaths[mr.Protocol]
	if !ok {
		return host.ModelStream{}, fmt.Errorf("model stream: unknown protocol %q", mr.Protocol)
	}
	if strings.TrimSpace(mr.Account) == "" {
		return host.ModelStream{}, errors.New("model stream: no account to pin")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := h.newGatewayRequest(streamCtx, http.MethodPost, path, bytes.NewReader(mr.Body))
	if err != nil {
		cancel()
		return host.ModelStream{}, fmt.Errorf("model stream: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(accountHeader, pinValue(mr.Account))
	if mr.Protocol == "claude" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := gatewayClient.Do(req)
	if err != nil {
		cancel()
		return host.ModelStream{}, fmt.Errorf("model stream: %w", err)
	}
	opened := host.ModelStream{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		resp.Body.Close()
		cancel()
		return opened, gatewayError("POST "+path, resp.StatusCode, body)
	}
	opened.ID = h.streams.add(&stream{body: resp.Body, cancel: cancel})
	return opened, nil
}

// ReadModelStream returns the next bytes of the body as they arrive: SSE
// in the protocol's own events, split anywhere. Done marks the end of the
// body. A failed read, including one cut by CloseModelStream, is an error.
func (h *Host) ReadModelStream(id string) (host.StreamChunk, error) {
	st := h.streams.get(id)
	if st == nil {
		return host.StreamChunk{}, fmt.Errorf("model stream %q is not open", id)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	buf := make([]byte, readChunkSize)
	n, err := st.body.Read(buf)
	chunk := host.StreamChunk{Payload: buf[:n]}
	switch {
	case errors.Is(err, io.EOF):
		chunk.Done = true
		return chunk, nil
	case err != nil:
		if n > 0 {
			// Hand over what arrived; the next read reports the error.
			return chunk, nil
		}
		return host.StreamChunk{}, fmt.Errorf("model stream: %w", err)
	}
	return chunk, nil
}

// CloseModelStream cancels the request and releases the stream. A
// concurrent ReadModelStream returns an error. Closing an unknown or closed
// stream does nothing.
func (h *Host) CloseModelStream(id string) error {
	st := h.streams.remove(id)
	if st == nil {
		return nil
	}
	st.cancel()
	return st.body.Close()
}
