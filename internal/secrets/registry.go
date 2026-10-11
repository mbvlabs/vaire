// Package secrets generates secrets and holds the process-wide redaction
// registry. Every output path passes through Redact before it reaches a
// reader.
package secrets

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// Placeholder replaces every registered secret value.
const Placeholder = "«redacted»"

// MinLength is the shortest value Add registers. Shorter values would redact
// ordinary words and numbers.
const MinLength = 6

// Registry is a mutex-guarded set of secret values.
type Registry struct {
	mu     sync.Mutex
	values map[string]struct{}
	sorted []string
}

// Default is the registry node mode uses for every output path.
var Default = NewRegistry()

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{values: map[string]struct{}{}}
}

// Add registers value. Values shorter than MinLength are ignored.
func (r *Registry) Add(value string) {
	if r == nil || len(value) < MinLength {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.values[value]; ok {
		return
	}
	r.values[value] = struct{}{}
	r.sorted = nil
}

// Values returns the registered values, longest first.
func (r *Registry) Values() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sortedLocked()...)
}

func (r *Registry) sortedLocked() []string {
	if r.sorted != nil || len(r.values) == 0 {
		return r.sorted
	}
	out := make([]string, 0, len(r.values))
	for value := range r.values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	r.sorted = out
	return out
}

// Redact replaces each registered value in s with Placeholder, longest first,
// so a secret that contains another one is hidden completely.
func (r *Registry) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.Lock()
	values := r.sortedLocked()
	r.mu.Unlock()
	for _, value := range values {
		if strings.Contains(s, value) {
			s = strings.ReplaceAll(s, value, Placeholder)
		}
	}
	return s
}

// RedactValue returns a JSON-shaped copy of v with every string, including map
// keys, redacted. Maps, slices and structs are walked through a JSON round
// trip, so the result marshals like v does.
func (r *Registry) RedactValue(v any) any {
	if v == nil {
		return nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return r.Redact(err.Error())
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return r.Redact(string(encoded))
	}
	return r.walk(generic)
}

func (r *Registry) walk(v any) any {
	switch value := v.(type) {
	case string:
		return r.Redact(value)
	case []any:
		for i := range value {
			value[i] = r.walk(value[i])
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[r.Redact(key)] = r.walk(item)
		}
		return out
	default:
		return value
	}
}

// Add registers value with Default.
func Add(value string) { Default.Add(value) }

// Redact redacts s with Default.
func Redact(s string) string { return Default.Redact(s) }

// RedactValue redacts v with Default.
func RedactValue(v any) any { return Default.RedactValue(v) }
