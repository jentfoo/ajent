package subagent

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
)

const (
	defaultMaxConcurrent = 8
	defaultPollTimeout   = 10 * time.Minute
	closeWait            = 2 * time.Second // bound on Close waiting for jobs to stop
	maxLabelLen          = 72              // single-line label truncation, in runes
)

// Options configures a sub-agent Manager. The func-typed fields are supplied by
// main.go so this package never imports pkg/tui or pkg/tools.
type Options struct {
	Provider            func(llm.Model) (llm.Provider, error)
	Model               func() llm.Model           // configured child model, else the session's
	Reasoning           func() llm.ReasoningConfig // inherited from the parent
	Parent              func() *tokens.Accounting  // parent ledger; Child() per job
	Tools               ToolSource                 // nil disables a child's tool set entirely
	Env                 agent.Environment
	ProjectInstructions []agent.ProjectInstruction

	Activity func(key, text string) // nil disables activity rows
	Notice   func(msg string)       // keyed UI notice for completions
	Status   func(text, short string)
	Deliver  func(agent.Input) bool // steer into a running parent turn; false when idle

	MaxConcurrent int           // 0 -> defaultMaxConcurrent
	PollTimeout   time.Duration // 0 -> defaultPollTimeout
}

// Manager owns every sub-agent job: spawning, concurrency bounding, polling,
// completion notification and shutdown.
type Manager struct {
	opts Options
	sem  chan struct{} // buffered to MaxConcurrent; send acquires a slot

	wg sync.WaitGroup

	mu      sync.Mutex
	jobs    map[string]*job
	pending []string // completed ids not yet delivered into the parent context
	count   int      // id counter -> sub-1, sub-2, …
}

// New returns a Manager with defaults resolved.
func New(opts Options) *Manager {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultMaxConcurrent
	}
	return &Manager{opts: opts, sem: make(chan struct{}, opts.MaxConcurrent), jobs: map[string]*job{}}
}

// Start launches one investigation and returns its id immediately. The job sits
// StatusQueued until it takes a concurrency slot.
func (m *Manager) Start(task, instructions string) string {
	var ledger *tokens.Accounting
	if p := m.opts.Parent; p != nil { // one child ledger per job, set before the id is visible to pollers
		ledger = p().Child()
	}
	m.mu.Lock()
	m.count++
	id := "sub-" + strconv.Itoa(m.count)
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{
		id:           id,
		task:         task,
		label:        shortLabel(task),
		instructions: instructions,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		tokens:       ledger,
	}
	m.jobs[id] = j
	m.mu.Unlock()

	j.setQueued(time.Now())
	// show the job immediately: rows render above the prompt even while queued,
	// before the child's turn emits anything (childSink publishes only on output).
	if fn := m.opts.Activity; fn != nil {
		fn(j.id, rowLine(id, j.label))
	}
	m.wg.Add(1) // before the goroutine so Close's Wait never races a pending Add
	go m.spawn(j)
	m.publishStatus()
	return id
}

// spawn drives one job to completion on its own goroutine. acquired gates the
// slot release: a queued job cancelled before acquiring never sent into sem, so
// releasing unconditionally would steal another running job's slot and over-admit.
func (m *Manager) spawn(j *job) {
	acquired := m.acquireSlot(j)
	defer func() {
		if acquired { // only a holder may free one; otherwise we exceed MaxConcurrent
			m.releaseSlot(j)
		}
		m.wg.Done()
	}()
	var sum string
	var err error
	switch {
	case !acquired: // cancelled while queued; never ran
		j.finish(StatusAborted, "", nil)
	default:
		j.markRunning()
		sum, err = m.run(j.ctx, j)
		switch {
		case j.ctx.Err() != nil: // user stop or shutdown beats any partial result
			j.finish(StatusAborted, sum, err)
		case err != nil:
			j.finish(StatusError, "", err)
		default:
			j.finish(StatusDone, sum, nil)
		}
	}
	// every terminal path clears the row Start published. Running jobs already did
	// via childSink.TurnEnd; this also covers a job that never acquired its slot.
	if fn := m.opts.Activity; fn != nil {
		fn(j.id, "")
	}
	m.publishStatus()
	close(j.done) // release waiting pollers before onComplete so their claim is visible
	m.onComplete(j)
}

// acquireSlot blocks until a concurrency slot frees or the job is cancelled.
func (m *Manager) acquireSlot(j *job) bool {
	select {
	case m.sem <- struct{}{}:
		return true
	case <-j.ctx.Done():
		return false
	}
}

// releaseSlot returns the job's concurrency slot, if it holds one.
func (m *Manager) releaseSlot(j *job) {
	select {
	case <-m.sem:
	default:
	}
}

// Poll blocks until id completes, PollTimeout elapses, or ctx is cancelled. It
// returns false when still running; an interrupted turn releases the poll at once.
func (m *Manager) Poll(ctx context.Context, id string) (Job, bool) {
	j, ok := m.lookup(id)
	if !ok {
		return Job{ID: normalizeID(id)}, false
	}
	timeout := m.opts.PollTimeout
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}

	j.mu.Lock()
	j.pollers++
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.pollers--
		j.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-j.done:
		// claim delivery so a later Flush/offer does not re-notify after this poll
		j.mu.Lock()
		j.consumed = true
		j.mu.Unlock()
		return j.snapshot(), true
	case <-timer.C:
		return j.snapshot(), false // still running; caller reads progress for the payload
	case <-ctx.Done():
		return Job{}, false
	}
}

// List returns a snapshot of every job, oldest id first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	ids := make([]string, 0, len(m.jobs))
	for k := range m.jobs {
		ids = append(ids, k)
	}
	slices.Sort(ids)
	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		if j, ok := m.jobs[id]; ok {
			out = append(out, j.snapshot())
		}
	}
	m.mu.Unlock()
	return out
}

// Stop cancels one job by id (accepts sub-2 or bare 2). A queued or running job is
// aborted; a finished one returns an error.
func (m *Manager) Stop(id string) error {
	j, ok := m.lookup(id)
	if !ok {
		return fmt.Errorf("no such sub-agent %q", id)
	}
	s := j.statusOf()
	if s == StatusDone || s == StatusError {
		return fmt.Errorf("sub-agent %s already finished (%s)", j.id, s)
	}
	j.cancel()
	return nil
}

// StopAll cancels every in-flight job and returns how many were cancelled.
func (m *Manager) StopAll() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0 // only in-flight jobs are actually cancelled; finished ones are not counted
	for _, j := range m.jobs {
		s := j.statusOf()
		if s == StatusQueued || s == StatusRunning {
			j.cancel()
			n++
		}
	}
	return n
}

// Flush retries delivering every completed-but-undelivered id into the parent
// context. Called from a turn-start observer so pending completions reach the
// model on the next real turn without ever starting one.
func (m *Manager) Flush() {
	m.mu.Lock()
	ids := slices.Clone(m.pending)
	m.mu.Unlock()
	if len(ids) > 0 {
		m.offer(ids)
	}
}

// Close cancels every job and waits briefly for them to stop, then clears the
// activity rows and status segment.
func (m *Manager) Close() {
	m.StopAll()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	timer := time.NewTimer(closeWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C: // jobs may be stuck on a slow provider; do not block shutdown
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.jobs))
	for k := range m.jobs {
		ids = append(ids, k)
	}
	m.pending = nil
	m.mu.Unlock()
	if fn := m.opts.Activity; fn != nil {
		for _, id := range ids {
			fn(id, "")
		}
	}
	if fn := m.opts.Status; fn != nil {
		fn("", "")
	}
}

// Tools returns the three agent_* tools a parent registers.
func (m *Manager) Tools() []agent.Tool {
	return []agent.Tool{&startTool{m: m}, &pollTool{m: m}, &listTool{m: m}}
}

// onComplete delivers completion to the user and, unless a poll is waiting or
// already consumed it, to the model. Aborts are silent.
func (m *Manager) onComplete(j *job) {
	j.mu.Lock()
	status := j.status
	pollers := j.pollers // a registered poller carries the result; a steer would waste tokens
	consumed := j.consumed
	id := j.id
	j.mu.Unlock()

	if status == StatusAborted || pollers > 0 || consumed {
		return
	}
	if fn := m.opts.Notice; fn != nil {
		fn("Sub-agent " + id + " completed")
	}
	m.offer([]string{id})
}

// offer marks ids pending and attempts to steer them into the running parent turn.
// A false Deliver (parent idle) leaves them pending for Flush; Delivered clears
// exactly the names a message carried, so one dropped by an interrupt is re-offered.
func (m *Manager) offer(ids []string) {
	var deliverable []string
	for _, id := range ids { // drop any taken by a poll between onComplete and here
		j, ok := m.lookup(id)
		if !ok {
			continue
		}
		j.mu.Lock()
		if j.pollers == 0 && !j.consumed {
			deliverable = append(deliverable, id)
		}
		j.mu.Unlock()
	}
	if len(deliverable) == 0 {
		return
	}

	m.mu.Lock()
	for _, id := range deliverable {
		if !slices.Contains(m.pending, id) {
			m.pending = append(m.pending, id)
		}
	}
	m.mu.Unlock()

	in := agent.Input{
		Text:     completionNotice(deliverable),
		Injected: true, // a system notice, not a typed prompt; excluded from recall
		Delivered: func() { // clear exactly the names this message carried
			m.mu.Lock()
			out := m.pending[:0]
			for _, p := range m.pending {
				if !slices.Contains(deliverable, p) {
					out = append(out, p)
				}
			}
			m.pending = out
			m.mu.Unlock()
		},
	}
	if fn := m.opts.Deliver; fn == nil || !fn(in) { // idle: ids stay pending and are re-offered by Flush
		return
	}
}

// completionNotice is the steer text naming completed sub-agents.
func completionNotice(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		id := ids[0]
		return "Sub-agent " + id + " completed. Call agent_poll with id " + id + " to retrieve the summary."
	default:
		return fmt.Sprintf("Sub-agents %s completed. Poll each for its summary.", strings.Join(ids, ", "))
	}
}

// publishStatus recomputes and pushes the status segment; empty clears it.
func (m *Manager) publishStatus() {
	fn := m.opts.Status
	if fn == nil {
		return
	}
	m.mu.Lock()
	var running, done int
	var oldest time.Duration
	for _, j := range m.jobs {
		s := j.statusOf()
		switch s {
		case StatusQueued, StatusRunning:
			running++
			if d := time.Since(j.started); d > oldest {
				oldest = d
			}
		case StatusDone:
			done++
		}
	}
	m.mu.Unlock()

	if running == 0 && done == 0 {
		fn("", "")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "subagents: %d running", running)
	if oldest > 0 {
		fmt.Fprintf(&b, " (oldest %s)", strutil.Elapsed(oldest))
	}
	if done > 0 {
		fmt.Fprintf(&b, ", %d done", done)
	}
	short := ""
	if running > 0 {
		short = fmt.Sprintf("sub %d", running)
	}
	fn(b.String(), short)
}

// model returns the configured child model or the session's current one.
func (m *Manager) model() llm.Model {
	if fn := m.opts.Model; fn != nil {
		return fn()
	}
	return llm.Model{}
}

// reasoning inherits the parent's reasoning configuration verbatim.
func (m *Manager) reasoning() llm.ReasoningConfig {
	if fn := m.opts.Reasoning; fn != nil {
		return fn()
	}
	return llm.ReasoningConfig{}
}

// pollProgress reads a still-running job's elapsed and child context usage for the
// timeout payload.
func (j *job) pollProgress() string {
	j.mu.Lock()
	elapsed := time.Since(j.started).Round(time.Second)
	id := j.id
	j.mu.Unlock()

	var used, win int
	if t := j.tokens; t != nil {
		c := t.Context()
		used, win = c.Used, c.Window
	}
	s := fmt.Sprintf("sub-agent %s still running after %s", id, strutil.Elapsed(elapsed))
	switch {
	case win > 0:
		return s + fmt.Sprintf(", context %d/%d tokens used against its window", used, win)
	case used > 0:
		return s + fmt.Sprintf(", about %d context tokens used", used)
	default:
		return s
	}
}

// shortLabel reduces task text to one rune-safe line.
func shortLabel(text string) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len([]rune(line)) <= maxLabelLen {
		return line
	}
	return strutil.Clip(line, maxLabelLen-1) // the ellipsis takes one rune of the budget
}

// normalizeID accepts sub-2 or bare 2.
func normalizeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "sub-") {
		return id
	}
	return "sub-" + id
}

// lookup resolves a job by id (accepting the bare form).
func (m *Manager) lookup(id string) (*job, bool) {
	n := normalizeID(id)
	if n == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[n]
	return j, ok
}
