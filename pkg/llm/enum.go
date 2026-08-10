package llm

import (
	"fmt"
	"strings"
)

// enumNames maps a small enum to the names used in configuration and logs.
// Enums encode as text rather than JSON so they work as map keys too, which is
// what thinkingLevelMap needs.
type enumNames[T ~uint8] map[T]string

// name returns the configuration name of v, or "unknown".
func (n enumNames[T]) name(v T) string {
	if s, ok := n[v]; ok {
		return s
	}
	return "unknown"
}

// marshalText encodes v as its configuration name.
func (n enumNames[T]) marshalText(v T) ([]byte, error) {
	s, ok := n[v]
	if !ok {
		return nil, fmt.Errorf("llm: cannot encode enum value %d", v)
	}
	return []byte(s), nil
}

// unmarshalText decodes a configuration name into out, naming what on failure.
func (n enumNames[T]) unmarshalText(data []byte, out *T, what string) error {
	if v, ok := n.lookup(string(data)); ok {
		*out = v
		return nil
	}
	return fmt.Errorf("llm: unknown %s %q, want one of %s", what, data, strings.Join(n.sorted(), ", "))
}

// lookup returns the enum value named by s, matched case insensitively.
func (n enumNames[T]) lookup(s string) (T, bool) {
	for v, name := range n {
		if strings.EqualFold(name, s) {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// sorted returns the configuration names in enum order.
func (n enumNames[T]) sorted() []string {
	out := make([]string, 0, len(n))
	for v := range len(n) {
		if s, ok := n[T(v)]; ok {
			out = append(out, s)
		}
	}
	return out
}
