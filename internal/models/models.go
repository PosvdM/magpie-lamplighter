// Package models caches the gateway model list and provides the helpers that
// order ignition candidates.
//
// The Magpie gateway lists the models of every account together, so a model
// on the list may not be served by every account. Candidates are therefore
// picked by name (see internal/providers), and the engine tries the next
// candidate when an account refuses one.
package models

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Lister caches the model list.
type Lister struct {
	TTL time.Duration

	mu      sync.Mutex
	cached  []string
	fetched time.Time
}

// List returns the cached model ids, or reads them with fetch when the cache
// is empty or older than TTL (10 minutes by default).
func (l *Lister) List(ctx context.Context, fetch func(context.Context) ([]string, error)) ([]string, error) {
	ttl := l.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	l.mu.Lock()
	if time.Since(l.fetched) < ttl && len(l.cached) > 0 {
		out := append([]string(nil), l.cached...)
		l.mu.Unlock()
		return out, nil
	}
	l.mu.Unlock()
	raw, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(raw))
	for _, id := range raw {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	l.mu.Lock()
	l.cached = append([]string(nil), ids...)
	l.fetched = time.Now()
	l.mu.Unlock()
	return ids, nil
}

var digits = regexp.MustCompile(`\d+`)

func recencyKey(model string) []int {
	parts := digits.FindAllString(model, -1)
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, _ := strconv.Atoi(part)
		out = append(out, n)
	}
	return out
}

func newerFirst(a, b string) bool {
	ka, kb := recencyKey(a), recencyKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			return ka[i] > kb[i]
		}
	}
	if len(ka) != len(kb) {
		return len(ka) > len(kb)
	}
	return strings.ToLower(a) > strings.ToLower(b)
}

// Newest returns the models matching predicate, newest first. Model families
// encode generation, version and date in numeric segments, so comparing the
// segments picks up new releases without listing them. predicate receives
// the model name in lower case.
func Newest(models []string, predicate func(string) bool) []string {
	var out []string
	for _, model := range models {
		if predicate(strings.ToLower(model)) {
			out = append(out, model)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return newerFirst(out[i], out[j]) })
	return out
}

// InOrder returns the models matching each predicate in turn, keeping the
// order of models within each predicate.
func InOrder(models []string, predicates ...func(string) bool) []string {
	var out []string
	for _, predicate := range predicates {
		for _, model := range models {
			if predicate(strings.ToLower(model)) {
				out = append(out, model)
			}
		}
	}
	return out
}

// Unique drops repeated models, keeping the first of each.
func Unique(models []string) []string {
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		if !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}
