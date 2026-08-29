package permit

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
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
		{"allow as verb", "allowing this would be unsafe", ClassUnsure},
		{"deny as verb", "this denies nothing", ClassUnsure},
		{"deny plain", "deny", ClassDeny},
		{"deny phrase", "DENY: it modifies the file", ClassDeny},
		{"denied", "denied - reaches the network", ClassDeny},
		{"unsure literal", "unsure", ClassUnsure},
		{"old readonly word", "readonly", ClassUnsure},
		{"old write word", "write", ClassUnsure},
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
