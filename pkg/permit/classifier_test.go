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
		{"plain", "readonly", ClassReadOnly},
		{"upper", "READONLY", ClassReadOnly},
		{"hyphenated", "read-only", ClassReadOnly},
		{"backticked", "`readonly`", ClassReadOnly},
		{"trailing dot", "readonly.", ClassReadOnly},
		{"prefix word", "readonly because it only lists files", ClassReadOnly},
		{"write plain", "write", ClassWrite},
		{"write phrase", "WRITE: modifies the file", ClassWrite},
		{"unsure literal", "unsure", ClassUnsure},
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

	fn := &countingFn{verdict: ClassReadOnly}
	c := NewCachedClassifier(fn.call)

	assert.Equal(t, ClassReadOnly, c.Classify(context.Background(), Subject{Name: "bash", Args: "stat a"}))
	assert.Equal(t, 1, fn.count())
	// an identical subject is served from cache without re-invoking the model.
	assert.Equal(t, ClassReadOnly, c.Classify(context.Background(), Subject{Name: "bash", Args: "stat a"}))
	assert.Equal(t, 1, fn.count())
}

func TestCachedClassifierDistinctCommandsMissCache(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassWrite}
	c := NewCachedClassifier(fn.call)

	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "a"})
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "b"}) // different subject misses
	assert.Equal(t, 2, fn.count())
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

	fn.verdict = ClassReadOnly // next call now succeeds and gets stored
	assert.Equal(t, ClassReadOnly, c.Classify(context.Background(), Subject{Name: "bash", Args: "stat b"}))
	_ = c.Classify(context.Background(), Subject{Name: "bash", Args: "stat b"}) // cached from here on
	assert.Equal(t, 3, fn.count())
}

func TestCachedClassifierEvictsLeastRecentlyUsedAtCap(t *testing.T) {
	t.Parallel()

	fn := &countingFn{verdict: ClassWrite}
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
