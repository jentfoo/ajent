package permit

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Class is a model classifier's verdict on one tool call.
type Class uint8

const (
	ClassAllow  Class = iota // safe to auto-allow under the rule set it was judged by
	ClassDeny                // writes or otherwise unsafe; keep the dialog open
	ClassUnsure              // garbled or failed response; never cached
)

// Subject is one call sent to the model classifier in auto/auto+write
// mode: a shell command, or any other (MCP/extension) tool named with its elided
// arguments.
type Subject struct {
	Name       string // tool name; bashTool for shell calls
	Args       string // bash command text, or elided JSON arguments for other tools
	Cwd        string // shell working directory when the call declares one
	AllowWrite bool   // judge under auto+write's workspace rules rather than read-only
}

// key is the cache identity: a call is judged by tool, exact payload, working
// directory and the rule set it was judged under, so different args to one MCP tool
// are classified independently and a mode change never reuses the other's verdict.
func (s Subject) key() string {
	return strconv.FormatBool(s.AllowWrite) + "\x00" + s.Name + "\x00" + s.Cwd + "\x00" + s.Args
}

// IsShell reports whether the subject is a shell command rather than an MCP/extension call.
func (s Subject) IsShell() bool { return s.Name == bashTool }

// Classifier decides whether an unverifiable tool call may run unattended.
// main.go supplies a fresh-context model adapter in the auto modes.
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

// NormalizeClass maps a classifier reply to a verdict. Every prompt answers
// allow/deny. An approval must be the reply's first whole word, so "allow, it only
// reads" approves but "allowing this would be unsafe" does not; the asymmetry with
// deny is deliberate, since only the approving direction can fail open.
func NormalizeClass(text string) Class {
	switch first := firstWord(text); first {
	case "allow", "allowed":
		return ClassAllow
	case "deny", "denied", "denies":
		return ClassDeny
	default:
		return ClassUnsure // indistinguishable from deny downstream; the dialog stays open
	}
}

// firstWord returns the leading run of letters, lowercased, so “ `Allow.` “ and
// "read-only" both reduce to one comparable token.
func firstWord(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	return b.String()
}
