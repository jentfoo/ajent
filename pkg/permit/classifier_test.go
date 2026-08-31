package permit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want Class
	}{
		{"plain", "allow", ClassAllow},
		{"upper", "ALLOW", ClassAllow},
		{"backticked", "`allow`", ClassAllow},
		{"trailing dot", "allow.", ClassAllow},
		{"approval with reason", "allow, it only lists files", ClassAllow},
		{"verdict after prose", "the command only reads; allow it", ClassAllow},
		{"deny plain", "deny", ClassDeny},
		{"approved word", "this is approved for the session", ClassAllow},
		{"disallowed word", "that command is disallowed here", ClassDeny},
		{"deny phrase", "DENY: it modifies the file", ClassDeny},
		{"denied with reason", "denied - reaches the network", ClassDeny},
		{"verdict after prose deny", "this would modify files, so deny", ClassDeny},
		{"conflicting allow and deny", "allow if safe but deny otherwise", ClassUnsure},
		{"hedged both words", "not sure; maybe allow, maybe deny", ClassUnsure},
		{"verb form not verdict", "allowing this would be unsafe", ClassUnsure},
		// a negator before an approval never fails open: the dialog stays.
		{"negated cannot allow", "I can't allow this because it's unsafe", ClassUnsure},
		{"negated do not allow", "do not allow this modification", ClassUnsure},
		{"negated semicolon clause", "I cannot allow; it writes to the network", ClassUnsure},
		{"negated never allowed", "never allowed: modifies files", ClassUnsure},
		// a negator anywhere shadows every verdict word, even across a clause boundary.
		{"negation does not reset", "I can't deny it reads; so allow it", ClassUnsure},
		{"unsure literal", "unsure", ClassUnsure},
		{"old readonly word", "readonly", ClassUnsure},
		{"garbled", "maybe? 42!", ClassUnsure},
		{"empty", "", ClassUnsure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, NormalizeClass(c.in))
		})
	}
}

// countingFn returns a canned verdict and counts invocations.
type countingFn struct {
	mu      sync.Mutex
	n       int
	verdict Class
}

func (f *countingFn) call(ctx context.Context, s Subject) Class {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.verdict
}
func (f *countingFn) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.n }

func TestCachedClassifierHitsCache(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassAllow}
	c := NewCachedClassifier(fn.call)

	assert.Equal(t, ClassAllow, c.Classify(context.Background(), Subject{Name: "bash", Args: "stat a"}))
	assert.Equal(t, 1, fn.count())
	// an identical subject is served from cache without re-invoking the model.
	assert.Equal(t, ClassAllow, c.Classify(context.Background(), Subject{Name: "bash", Args: "stat a"}))
	assert.Equal(t, 1, fn.count())
}

func TestCachedClassifierDistinctCommandsMissCache(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassDeny}
	c := NewCachedClassifier(fn.call)

	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "a"})
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "b"}) // different subject misses
	assert.Equal(t, 2, fn.count())
}

func TestCachedClassifierSeparatesRuleSets(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassDeny}
	c := NewCachedClassifier(fn.call)

	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "rm f"})
	assert.Equal(t, 1, fn.count())
	// the same command under auto+write asks a different question; never the cached one.
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "rm f", AllowWrite: true})
	assert.Equal(t, 2, fn.count())
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "rm f", AllowWrite: true})
	assert.Equal(t, 2, fn.count()) // cached within its own rule set
}

func TestCachedClassifierNeverStoresUnsure(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassUnsure}
	c := NewCachedClassifier(fn.call)

	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "stat a"})
	assert.Equal(t, 1, fn.count())
	// unsure is transient and never cached; the same subject runs the model again.
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "stat a"})
	assert.Equal(t, 2, fn.count())

	fn.verdict = ClassAllow // next call now succeeds and gets stored
	assert.Equal(t, ClassAllow, c.Classify(context.Background(), Subject{Name: "bash", Args: "stat b"}))
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "stat b"}) // cached from here on
	assert.Equal(t, 3, fn.count())
}

// gatedFn holds each classification open until released or its context ends,
// counting starts; the leading fails calls answer unsure, the rest verdict.
type gatedFn struct {
	release chan struct{}
	verdict Class
	fails   int

	mu     sync.Mutex
	starts int
	ends   int
}

func (f *gatedFn) call(ctx context.Context, _ Subject) Class {
	f.mu.Lock()
	f.starts++
	i := f.starts
	f.mu.Unlock()
	var v Class
	select {
	case <-f.release:
		if i <= f.fails {
			v = ClassUnsure
		} else {
			v = f.verdict
		}
	case <-ctx.Done():
		v = ClassUnsure
	}
	f.mu.Lock()
	f.ends++
	f.mu.Unlock()
	return v
}
func (f *gatedFn) startedN() int { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }
func (f *gatedFn) endedN() int   { f.mu.Lock(); defer f.mu.Unlock(); return f.ends }

// collect gathers n classes from ch without sleeping.
func collect(t *testing.T, ch <-chan Class, n int) []Class {
	t.Helper()
	var out []Class
	require.Eventually(t, func() bool {
		for {
			select {
			case v := <-ch:
				out = append(out, v)
				if len(out) == n {
					return true
				}
			default:
				return false
			}
		}
	}, time.Second, 5*time.Millisecond)
	return out
}

// TestCachedClassifierInFlight covers the single-flight join: concurrent
// identical subjects share one model request, a cancelled joiner takes unsure
// without disturbing the leader's call, and a failed leader's unsure never
// decides for a caller that still waits.
func TestCachedClassifierInFlight(t *testing.T) {
	t.Parallel()

	t.Run("joins concurrent callers", func(t *testing.T) {
		fn := &gatedFn{release: make(chan struct{}), verdict: ClassAllow}
		c := NewCachedClassifier(fn.call)

		const callers = 4
		res := make(chan Class, callers)
		for range callers {
			go func() { res <- c.Classify(t.Context(), Subject{Name: "bash", Args: "rm f"}) }()
		}
		// one leader runs; the rest join it instead of issuing their own request
		require.Eventually(t, func() bool { return fn.startedN() == 1 }, time.Second, 5*time.Millisecond)
		close(fn.release)

		for _, v := range collect(t, res, callers) {
			assert.Equal(t, ClassAllow, v)
		}
		assert.Equal(t, 1, fn.startedN())
		assert.Equal(t, 1, fn.endedN())
	})

	t.Run("cancelled joiner takes unsure", func(t *testing.T) {
		fn := &gatedFn{release: make(chan struct{}), verdict: ClassAllow}
		c := NewCachedClassifier(fn.call)
		subj := Subject{Name: "bash", Args: "rm f"}

		go func() { c.Classify(t.Context(), subj) }() // the leader
		require.Eventually(t, func() bool { return fn.startedN() == 1 }, time.Second, 5*time.Millisecond)

		// a joiner mirrors an asker whose dialog the user answered: cancel its context
		joinCtx, cancel := context.WithCancel(t.Context())
		joined := make(chan Class, 1)
		go func() { joined <- c.Classify(joinCtx, subj) }()
		cancel()
		assert.Equal(t, []Class{ClassUnsure}, collect(t, joined, 1))

		close(fn.release) // the leader still finishes and caches its verdict
		require.Eventually(t, func() bool { return fn.endedN() == 1 }, time.Second, 5*time.Millisecond)
		assert.Equal(t, ClassAllow, c.Classify(t.Context(), subj)) // served from cache
		assert.Equal(t, 1, fn.startedN())
	})

	t.Run("retries after failed leader", func(t *testing.T) {
		// the first call answers unsure (a cancelled leader); the retry gets allow
		fn := &gatedFn{release: make(chan struct{}), verdict: ClassAllow, fails: 1}
		c := NewCachedClassifier(fn.call)
		subj := Subject{Name: "bash", Args: "rm f"}

		res := make(chan Class, 2)
		for range 2 {
			go func() { res <- c.Classify(t.Context(), subj) }()
		}
		require.Eventually(t, func() bool { return fn.startedN() == 1 }, time.Second, 5*time.Millisecond)
		close(fn.release)

		// the failed leader's unsure never decides for the caller that still waits
		assert.ElementsMatch(t, []Class{ClassUnsure, ClassAllow}, collect(t, res, 2))
		assert.Equal(t, 2, fn.startedN())
		assert.Equal(t, ClassAllow, c.Classify(t.Context(), subj)) // the retry cached its verdict
		assert.Equal(t, 2, fn.startedN())
	})
}

func TestCachedClassifierEvictsLeastRecentlyUsedAtCap(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassDeny}
	c := newCachedClassifierMax(fn.call, 2)

	// fill the cache to its cap.
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "a"})
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "b"})

	// touch a so b becomes least-recently-used, then push past the cap: b evicts.
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "a"})
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "c"}) // forces an eviction

	assert.Equal(t, 3, fn.count()) // a,b,c each classified once
	// b was evicted and must run the model again; a survived via recency touch.
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "b"})
	assert.Equal(t, 4, fn.count())
}
