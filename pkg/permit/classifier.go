package permit

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Class is a model classifier's verdict on one tool call.
type Class uint8

const (
	ClassAllow  Class = iota // safe to auto-allow under the rule set it was judged by
	ClassDeny                // writes or otherwise unsafe; keep the dialog open
	ClassUnsure              // garbled or failed response; never cached
)

// Subject is one call sent to the model classifier in the auto modes.
type Subject struct {
	Name       string // tool name; bashTool for shell calls
	Args       string // bash command text, or elided JSON arguments for other tools
	Cwd        string // shell working directory when the call declares one
	AllowWrite bool   // judge under auto+write's workspace rules rather than read-only
}

// key is the cache identity: a call is judged by tool, exact payload, working
// directory and rule set, so a mode change never reuses the other's verdict.
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

// cachedClassifier wraps an uncached classifier with a session-scoped LRU keyed
// by subject identity (tool + exact payload). Concurrent identical subjects share
// one in-flight request, so batch prefetch and the dialogs it fronts never issue
// duplicate model calls. unsure verdicts are never stored; they are usually
// transient (an abort, missing auth, an API error).
type cachedClassifier struct {
	fn ClassifierFn

	mu       sync.Mutex
	max      int                       // cache cap, classCacheMax for production use
	vals     map[string]Class          // subject key -> verdict
	order    []string                  // least-recently-used first; the tail is most recent
	inflight map[string]*inflightClass // subject key -> running call joiners wait on
}

// inflightClass is one running classification that identical concurrent callers
// join instead of issuing their own model request. class is set before done closes.
type inflightClass struct {
	done  chan struct{}
	class Class
}

// NewCachedClassifier returns a session-scoped, LRU-caching classifier over fn.
func NewCachedClassifier(fn ClassifierFn) *cachedClassifier {
	return newCachedClassifierMax(fn, classCacheMax)
}

// newCachedClassifierMax builds a caching classifier with a custom cap for tests.
func newCachedClassifierMax(fn ClassifierFn, max int) *cachedClassifier {
	return &cachedClassifier{fn: fn, max: max,
		vals: make(map[string]Class), inflight: make(map[string]*inflightClass)}
}

// Classify serves a cached verdict, joins an identical in-flight call, or leads
// it with fn. A joiner whose context ends takes unsure; one handed a failed
// (cancelled) leader retries while its own context lives, so a cancelled
// predecessor never decides for a caller still waiting.
func (c *cachedClassifier) Classify(ctx context.Context, s Subject) Class {
	key := s.key()
	for ctx.Err() == nil { // keep joining while predecessors fail on dead contexts
		c.mu.Lock()
		if v, ok := c.vals[key]; ok {
			touch(c.order, key)
			c.mu.Unlock()
			return v
		}
		fl, ok := c.inflight[key]
		if !ok {
			fl = &inflightClass{done: make(chan struct{})}
			c.inflight[key] = fl
		}
		c.mu.Unlock()

		if !ok { // leader: our verdict decides for everyone joining us
			v := c.fn(ctx, s)
			if v != ClassUnsure {
				c.mu.Lock()
				c.storeLocked(key, v)
				c.mu.Unlock()
			}
			fl.class = v
			c.mu.Lock()
			delete(c.inflight, key)
			c.mu.Unlock()
			close(fl.done)
			return v
		}
		select {
		case <-fl.done:
			if fl.class != ClassUnsure {
				return fl.class
			}
			// the predecessor failed on a dead context; lead (or join) again ourselves
		case <-ctx.Done():
			return ClassUnsure
		}
	}
	return ClassUnsure
}

// storeLocked records v under key at the LRU tail, evicting at capacity. Caller
// holds the lock; only the key's in-flight leader stores.
func (c *cachedClassifier) storeLocked(key string, v Class) {
	if _, ok := c.vals[key]; ok {
		touch(c.order, key) // already present; refresh recency only
		return
	}
	if len(c.order) >= c.max {
		delete(c.vals, c.order[0]) // evict the least-recently-used entry
		c.order = slices.Delete(c.order, 0, 1)
	}
	c.vals[key] = v
	c.order = append(c.order, key)
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

// classNegators shadow a following verdict word, so "I can't allow this" is never
// read as an approval. Apostrophes stay inside words here, unlike the letter-only
// splitter below, so contractions match whole.
var classNegators = map[string]bool{
	"not": true, "never": true, "cannot": true,
	"can't": true, "cant": true, "don't": true, "dont": true,
	"won't": true, "wont": true, "isn't": true, "isnt": true,
}

// NormalizeClass maps a classifier reply to a verdict by the marker words it
// contains anywhere: one direction yields its class; both or neither is unsure.
// Only an unambiguous approval can fail open, so any conflict keeps the dialog
// open and a negator shadowing a verdict word ("can't allow") counts for nothing.
func NormalizeClass(text string) Class {
	var hasAllow, hasDeny, negated bool
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		switch w {
		case "allow", "allowed", "approved":
			hasAllow = true
		case "deny", "denied", "denies", "disallowed":
			hasDeny = true
		}
		if classNegators[w] {
			negated = true // a negator anywhere shadows every verdict word: never fail open
		}
	}
	if hasAllow != hasDeny && !negated {
		if hasAllow {
			return ClassAllow
		}
		return ClassDeny
	}
	return ClassUnsure // both, neither, or a shadowing negator: the dialog stays open
}
