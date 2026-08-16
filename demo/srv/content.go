package srv

import (
	"fmt"
	"strings"
)

// unwrap joins the source wrapped lines of each paragraph into a single long
// line, so the terminal decides where to break rather than this file. Markdown
// gets this for free through soft line breaks; plain text such as thinking does not.
func unwrap(s string) string {
	paragraphs := strings.Split(strings.TrimSuffix(s, "\n"), "\n\n")
	for i, p := range paragraphs {
		paragraphs[i] = strings.Join(strings.Split(p, "\n"), " ")
	}
	return strings.Join(paragraphs, "\n\n") + "\n"
}

// expandTicks turns @@@ into a code fence and @@ into an inline code span, since
// Go raw strings cannot contain backticks.
func expandTicks(s string) string {
	s = strings.ReplaceAll(s, "@@@", "```")
	return strings.ReplaceAll(s, "@@", "`")
}

var demoThinking = unwrap(`The retry helper in pkg/client/retry.go loops a fixed number of times with no
backoff and no way to cancel, so a hung call blocks the whole agent loop.

Plan: thread a context through the call, add exponential backoff between
attempts, then cover both with a table driven test. The cap needs its own case
since an unbounded shift overflows the duration after about twenty attempts.
`)

//nolint:gosmopolitan // wide script samples are deliberate, they exercise the width math
var demoReply = expandTicks(`## Retry hardening

I threaded a **context** through @@retry@@ and added exponential backoff, so a
hung call can no longer block the whole agent loop.

### What changed

- ✅ **Cancellation** so the loop returns as soon as the context is done
- ⏱️ **Backoff** where @@backoff(i)@@ grows 50ms, 100ms, 200ms, capped at 2s
- 🧪 **Tests** with one table driven case per failure mode

The cap lives inside @@backoff@@ itself, so every caller gets it for free:

@@@go
func backoff(attempt int) time.Duration {
	if attempt > maxShift {
		return maxDelay
	}
	return min(baseDelay<<attempt, maxDelay)
}
@@@

| Case | Attempts | Result |
|---|---:|---|
| first call succeeds | 1 | ✅ nil |
| transient failure | 3 | ✅ nil |
| context cancelled | 1 | ❌ context.Canceled |
| 日本語のケース | 2 | ✅ nil |

> The cap matters more than the base delay here, an unbounded shift overflows
> the duration after about twenty attempts.

----

### Follow ups

1. Thread the same context through @@client.Do@@
2. Replace the *fixed* attempt count with a deadline
3. See [the retry notes](https://ajent.dev/retry) for the rationale

Nothing here changes the public API, so ~~a major version bump~~ a patch
release is enough. 🚀

Unicode widths are handled by grapheme cluster, so wide scripts (日本語, 中文,
한국어), combining marks (é vs é), emoji with modifiers (👍🏽, 👨‍👩‍👧‍👦) and
symbols (→ ✓ ∑ ≈ °C) all measure and wrap correctly.
`)

var demoWrapUp = unwrap(`Full suite is green across all five packages ✅. The overflow case at attempt 40
covers the cap, so the shift can no longer wrap around. Try resizing the window,
the prose above should re-wrap while tables, diffs and code keep their shape.
`)

// demoThinking0 is step 0's brief plan before creating the scratch workspace.
var demoThinking0 = unwrap(`Set up an isolated scratch directory under /tmp so the whole run stays confined to
a path I can clean up in one shot at the end.`)

// demoThinking2 explains why step 3's read follows a failed edit: compare exact
// text rather than guessing at the file contents again.
var demoThinking2 = unwrap(`The first edit reported "oldText not found", so something about my assumption was
off. Rather than guess again, I should re-read the file and see what is actually
there before composing an exact replacement.
`)

// retryBefore is present in notes.go; step 4's successful edit replaces it.
var retryBefore = `func backoff(attempt int) time.Duration {
	if attempt > maxShift {
		return maxDelay
	}
	return min(baseDelay<<attempt, maxDelay)
}`

// retryAfter is what step 4 turns that snippet into: a doc comment plus an early
// cap check so the shift can never overflow.
var retryAfter = `func backoff(attempt int) time.Duration {
	if attempt > maxShift {
		return maxDelay // capped, so an unbounded shift cannot overflow
	}
	d := min(baseDelay<<attempt, maxDelay)
	if d < baseDelay { // a wrapped shift means the cap is far below
		return maxDelay
	}
	return d
}`

// missingOldText is step 2's deliberately wrong edit target: it does not exist in
// notes.go, so DryRun fails and the barrier lets it through without prompting.
const missingOldText = `func legacyRetry(n int) error {
	for i := 0; i < n; i++ {
		if err := call(); err == nil {
			return nil
		}
	}
	return errFailed
}`

// notesGo is the ~180-line plausible Go file step 1 writes into the scratch dir;
// its backoff body matches retryBefore so step 4's edit can replace it.
var notesGo = `package notes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Tunables shared by every Retrier and Client in the package.
const (
	maxShift       = 5              // doubling stops here so a shift cannot overflow
	baseDelay      = 50 * time.Millisecond
	maxDelay       = 2 * time.Second
	maxAttempts    = 6
	defaultTimeout = 10 * time.Second
)

// Retrier runs a call up to retries times, sleeping an exponential backoff
// between attempts. It is safe for concurrent use once configured.
type Retrier struct {
	base    time.Duration // first interval
	capped  time.Duration // ceiling the growth stops at
	retries int           // total attempts including the initial one
}

// NewRetrier returns a Retrier with the package defaults: a 50ms base, a 2s
// cap and six attempts.
func NewRetrier() *Retrier {
	return &Retrier{base: baseDelay, capped: maxDelay, retries: maxAttempts}
}

// WithBase overrides the starting backoff interval; it must be positive.
func (r *Retrier) WithBase(d time.Duration) *Retrier { r.base = d; return r }

// WithMaxAttempts caps how many times Run invokes fn before giving up.
func (r *Retrier) WithMaxAttempts(n int) *Retrier { r.retries = n; return r }

// Budget reports how many attempts a fresh retrier will make.
func (r *Retrier) Budget() int { return r.retries }

// Run calls fn until it returns nil, the context is done or the attempt budget
// runs out. The final error names how many attempts were made.
func (r *Retrier) Run(ctx context.Context, fn func(context.Context) error) error {
	for i := 0; ; i++ {
		if err := fn(ctx); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else if i >= r.retries-1 {
			err := lastErr(fn)
			return fmt.Errorf("giving up after %d attempts: %w", i+1, err)
		}
		if err := sleep(ctx, backoff(i)); err != nil {
			return err
		}
	}
}

func backoff(attempt int) time.Duration {
	if attempt > maxShift {
		return maxDelay
	}
	return min(baseDelay<<attempt, maxDelay)
}

// sleep waits for d unless the context is done first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// lastErr re-invokes fn once to capture its final error for the wrap-up message.
func lastErr(fn func(context.Context) error) error { return fn(context.Background()) }

// Client wraps an http.Client with a Retrier around each request's Do call so
// transient network failures are retried transparently.
type Client struct {
	h *http.Client
	r *Retrier
}

// NewClient returns a Client using the shared Retrier defaults and a 10s overall
// timeout on its underlying transport.
func NewClient() *Client {
	return &Client{h: &http.Client{Timeout: defaultTimeout}, r: NewRetrier()}
}

// Do performs one retried request, returning the response. The caller owns
// closing resp.Body when err is nil.
func (c *Client) Do(ctx context.Context, method, url string) (*http.Response, error) {
	var (
		resp *http.Response
		err  error
	)
	runErr := c.r.Run(ctx, func(cx context.Context) error {
		req, err := http.NewRequestWithContext(cx, method, url, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		resp, err = c.h.Do(req)
		return err
	})
	if runErr != nil {
		err = runErr
	}
	return resp, err
}

// Get performs a retried GET and returns the response body length.
func (c *Client) Get(ctx context.Context, url string) (int, error) {
	resp, err := c.Do(ctx, http.MethodGet, url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if !okStatus(resp.StatusCode) {
		return 0, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	n := int(resp.ContentLength)
	if n < 0 { // chunked encoding; read to learn the size
		buf := &strings.Builder{}
		if _, err := io.Copy(buf, resp.Body); err != nil {
			return 0, err
		}
		n = buf.Len()
	}
	return n, nil
}

// okStatus reports whether code is a success class (2xx or 3xx).
func okStatus(code int) bool { return code >= 200 && code < 400 }

// ErrRateLimited marks a response the caller should back off from rather than
// retry immediately.
var ErrRateLimited = errors.New("rate limited")

// Post performs a retried POST with body and reports whether it succeeded. The
// request is idempotent only when body is nil, so callers must supply their own
// retry discipline otherwise.
func (c *Client) Post(ctx context.Context, url string, body io.Reader) error {
	_, err := c.DoWithBody(ctx, http.MethodPost, url, body)
	return err
}

// DoWithBody is Do but with an explicit request body; the reader is consumed on
// every retried attempt, so it should be a fresh bytes.Reader each time.
func (c *Client) DoWithBody(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	var resp *http.Response
	err := c.r.Run(ctx, func(cx context.Context) error {
		req, err := http.NewRequestWithContext(cx, method, url, body)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		resp, err = c.h.Do(req)
		return err
	})
	if err != nil {
		return resp, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		_ = resp.Body.Close()
		return nil, ErrRateLimited
	}
	return resp, nil
}

// IsRetryable distinguishes transient errors worth retrying from fatal ones.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrRateLimited) {
		return false
	}
	s := strings.ToLower(err.Error())
	return !strings.Contains(s, "not found")
}
`

// retryTestGo is a ~55-line test file written into the scratch dir so later read
// steps have a second real source to inspect.
var retryTestGo = `package notes

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackoffCaps(t *testing.T) {
	if got := backoff(maxShift + 1); got != maxDelay {
		t.Fatalf("backoff(%d) = %v, want cap %v", maxShift+1, got, maxDelay)
	}
}

func TestBackoffGrows(t *testing.T) {
	prev := time.Duration(0)
	for i := 0; i <= maxShift; i++ {
		d := backoff(i)
		if d < prev || d > maxDelay {
			t.Fatalf("backoff(%d)=%v out of range", i, d)
		}
		prev = d
	}
}

func TestRetrySucceedsEventually(t *testing.T) {
	r := NewRetrier().WithMaxAttempts(3)
	calls := 0
	err := r.Run(context.Background(), func(ctx context.Context) error {
		calls++
		if calls < 2 {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
`

// readmeMD is a short doc written so the demo has three distinct files to read.
const readmeMD = `# notes

A tiny retry helper with exponential backoff and a cap that cannot overflow the
shift. See notes.go for the implementation, retry_test.go for coverage and this
file for usage.
`

// summaryMarkdown renders step 10's closing prose with the measured run time.
func summaryMarkdown(total string) string {
	return expandTicks(fmt.Sprintf(`## Demo complete

The whole agent ran against a scripted model service: @@mkdir@@, a @@write@@,
two @@edit@@ attempts (one deliberate failure), @@read@@/@@find@@/@@grep@@ and
a pair of parallel calls, then the shell walkthrough.

No LLM was involved, so wall-clock time is harness overhead:

**total: %ss**

Resize the window to watch prose re-flow while tables, diffs and code hold their
shape. The script removed its scratch directory in a final @@rm -rf@@ call.
`, total))
}
