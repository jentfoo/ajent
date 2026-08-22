package permit

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// Class is a model classifier's verdict on one tool call.
type Class uint8

const (
	ClassReadOnly Class = iota // verifiably read-only, safe to auto-allow
	ClassWrite                 // writes or otherwise unsafe; keep the dialog open
	ClassUnsure                // garbled or failed response; never cached
)

// Subject is one call sent to the model classifier in auto/auto+mcp mode: a shell
// command, or any other (MCP/extension) tool named with its elided arguments.
type Subject struct {
	Name string // tool name; bashTool for shell calls
	Args string // bash command text, or elided JSON arguments for other tools
}

// key is the cache identity: a call is judged by tool and exact payload together,
// so different args to one MCP tool are classified independently.
func (s Subject) key() string { return s.Name + "\x00" + s.Args }

// IsShell reports whether the subject is a shell command rather than an MCP/extension call.
func (s Subject) IsShell() bool { return s.Name == bashTool }

// Classifier decides whether an unverifiable tool call is read-only. main.go
// supplies a fresh-context model adapter in auto/auto+mcp mode.
type Classifier interface {
	Classify(ctx context.Context, s Subject) Class
}

// classCacheMax bounds the session LRU so it stays cheap and forgets old commands.
const classCacheMax = 500

// ClassifierFn is one uncached classification; main.go supplies the model call.
type ClassifierFn func(ctx context.Context, s Subject) Class

// cachedClassifier wraps an uncached classifier with a session-scoped LRU keyed by
// subject identity (tool + exact payload). unsure verdicts are never stored; they
// are usually transient (an abort, missing auth, an API error).
type cachedClassifier struct {
	fn ClassifierFn

	mu    sync.Mutex
	max   int              // cache cap, classCacheMax for production use
	vals  map[string]Class // subject key -> verdict
	order []string         // least-recently-used first; the tail is most recent
}

// NewCachedClassifier returns a session-scoped, LRU-caching classifier over fn.
func NewCachedClassifier(fn ClassifierFn) *cachedClassifier {
	return newCachedClassifierMax(fn, classCacheMax)
}

// newCachedClassifierMax builds a caching classifier with a custom cap for tests.
func newCachedClassifierMax(fn ClassifierFn, max int) *cachedClassifier {
	return &cachedClassifier{fn: fn, max: max, vals: make(map[string]Class)}
}

// Classify consults the cache and otherwise runs fn, storing non-unsure verdicts.
func (c *cachedClassifier) Classify(ctx context.Context, s Subject) Class {
	key := s.key()
	c.mu.Lock()
	if v, ok := c.vals[key]; ok {
		touch(c.order, key)
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()

	v := c.fn(ctx, s)
	if v == ClassUnsure { // unsure is usually transient; never cached
		return v
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.vals[key]; !ok {
		if len(c.order) >= c.max {
			delete(c.vals, c.order[0]) // evict the least-recently-used entry
			c.order = slices.Delete(c.order, 0, 1)
		}
		c.vals[key] = v
		c.order = append(c.order, key)
	} else {
		touch(c.order, key) // another caller cached it meanwhile; refresh recency
	}
	return v
}

// touch moves key to the tail (most-recently-used), keeping eviction at index 0.
func touch(order []string, key string) {
	i := slices.Index(order, key)
	if i < 0 || i == len(order)-1 {
		return // absent or already most recent
	}
	copy(order[i:], order[i+1:]) // shift left over the removed slot
	order[len(order)-1] = key
}

// NormalizeClass maps a model's raw reply to a verdict: lowercase, drop every
// non-letter, then prefix-match so "read-only", `readonly.` and backticked answers
// all collapse cleanly. Anything else is ClassUnsure.
func NormalizeClass(text string) Class {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		}
	}
	cleaned := b.String()
	if strings.HasPrefix(cleaned, "readonly") {
		return ClassReadOnly
	}
	if strings.HasPrefix(cleaned, "write") {
		return ClassWrite
	}
	return ClassUnsure
}
