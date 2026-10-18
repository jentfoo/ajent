package subagent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
)

const (
	defaultMaxConcurrent = 8
	defaultPollTimeout   = 10 * time.Minute
	defaultCloseWait     = 2 * time.Second // bound on Close waiting for jobs to stop
	maxLabelLen          = 72              // single-line label truncation, in runes
)

// Options configures a sub-agent Manager. The func-typed fields are supplied
// at wiring time, so this package never imports pkg/tui or pkg/tools. Model,
// Reasoning, Parent, Notice, Deliver and Tools are required. Activity and
// Status are optional.
type Options struct {
	Provider            func(llm.Model) (llm.Provider, error)
	Model               func() llm.Model           // configured child model, else the session's
	Reasoning           func() llm.ReasoningConfig // inherited from the parent
	Parent              func() *tokens.Accounting  // parent ledger, one child per job
	Tools               ToolSource                 // parent registry view a child's read-only set filters
	Env                 agent.Environment
	ProjectInstructions []agent.ProjectInstruction

	// Root seeds the manager's context root: every job derives from it and Close
	// cancels it, so process shutdown reaches jobs directly.
	Root context.Context

	// Activity publishes one keyed row, removed by empty text. rank is the
	// job number, so rows hold a stable place regardless of publish order.
	Activity func(key, text string, rank int) // nil disables activity rows
	Notice   func(msg string)                 // keyed UI notice for completions
	Status   func(text, short string)
	Deliver  func(agent.Input) bool // steers into a running parent turn, false when idle

	MaxConcurrent int           // 0 -> defaultMaxConcurrent
	PollTimeout   time.Duration // 0 -> defaultPollTimeout
	MaxSteps      int           // per-turn step cap for every child; <= 0 means unlimited
}

// Manager owns every sub-agent job: spawning, concurrency bounding, polling,
// completion notification and shutdown.
type Manager struct {
	opts Options

	ctx    context.Context // root for every job's context, canceled on Close
	cancel context.CancelFunc

	wg sync.WaitGroup

	pubMu sync.Mutex // serializes each keyed publish (status segment, notice) with its snapshot

	closeWait time.Duration // bound on Close waiting for jobs, shortened in tests
	closed    bool          // set under pubMu by Close: no UI publication may follow its clears

	mu          sync.Mutex
	jobs        map[string]*job
	waiters     []*job         // registered but ungranted jobs, dispatched in job-number order
	running     int            // concurrency slots currently held
	pending     []string       // completed ids not yet delivered into the parent context
	inFlight    []string       // ids a queued steer names, cleared when it lands or drops
	noticeBatch []string       // completions since the last delivered steer, for the keyed notice
	count       int            // id counter, ids read as sub-N
	reserved    map[string]int // agent_start call id -> its number, from the ordered batch
	polling     int            // Poll calls in flight, for spotting a simultaneous batch
	batched     bool           // two polls have overlapped since the group last emptied
}

// New returns a Manager with defaults resolved.
func New(opts Options) *Manager {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultMaxConcurrent
	}
	m := &Manager{
		opts:      opts,
		jobs:      map[string]*job{},
		reserved:  map[string]int{},
		closeWait: defaultCloseWait,
	}
	m.ctx, m.cancel = context.WithCancel(opts.Root)
	return m
}

// Reserve hands out an id number for every agent_start in one tool batch, in the
// order the model asked for them. agent_start is ModeParallel, so without this
// the goroutines race for the counter and the task submitted last can become
// sub-1. A batch supersedes the previous one, dropping reservations whose call
// never ran (an interrupted turn), and the ids they held are simply skipped.
func (m *Manager) Reserve(calls []agent.ToolCall) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.reserved) // a batch supersedes the last, since a stale entry would misnumber
	for _, c := range calls {
		if c.Name != startToolName || c.ID == "" {
			continue
		}
		m.count++
		m.reserved[c.ID] = m.count
	}
}

// start launches one investigation whose id may have been reserved by callID, an
// unknown or empty callID taking the next number so a host-driven start still works.
func (m *Manager) start(task, instructions, callID string) string {
	ledger := m.opts.Parent().Child() // one child ledger per job, set before the id is visible to pollers
	m.mu.Lock()
	num, ok := m.reserved[callID]
	if ok {
		delete(m.reserved, callID)
	} else {
		m.count++
		num = m.count
	}
	id := "sub-" + strconv.Itoa(num)
	// derived from the manager root, so Close and an app-root cancel reach every
	// job even where StopAll's snapshot missed it
	ctx, cancel := context.WithCancel(m.ctx)
	j := &job{
		id:           id,
		num:          num,
		task:         task,
		label:        shortLabel(task),
		instructions: instructions,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		grant:        make(chan struct{}),
		tokens:       ledger,
	}
	m.jobs[id] = j
	j.setQueued(time.Now())
	if m.ctx.Err() != nil { // shutdown already ran (Close or an app-root cancel): register
		// aborted without a goroutine, so the WaitGroup can never race its Add against
		// Close's Wait; every Add before this point happened under mu ahead of any Wait
		j.finish(StatusAborted, "", nil)
		close(j.done)
		cancel() // parent already cancelled, but drop the registration for uniformity
		m.mu.Unlock()
		return id
	}
	m.wg.Add(1) // before the goroutine and under mu, so Close's Wait never races a pending Add
	// queued at registration, so activation follows job numbers not start races
	m.waiters = append(m.waiters, j)
	m.dispatchLocked()
	m.mu.Unlock()

	// show the job immediately: rows render above the prompt even while queued,
	// before the child's turn emits anything (childSink publishes only on output).
	m.publishActivity(j.id, rowLine(id, j.label), j.num)
	go m.spawn(j)
	m.publishStatus()
	return id
}

// spawn drives one job to completion on its own goroutine. acquired gates the
// slot release: a queued job cancelled before acquiring never held one, so
// releasing unconditionally would steal another running job's slot and over-admit.
func (m *Manager) spawn(j *job) {
	acquired := m.acquireSlot(j)
	defer func() {
		if acquired { // only a holder may free one, or we exceed MaxConcurrent
			m.releaseSlot()
		}
		j.cancel() // drop the root registration; jobs deriving from m.ctx hold it until cancelled
		m.wg.Done()
	}()
	var sum string
	var err error
	switch {
	case !acquired: // cancelled while queued, so it never ran
		j.finish(StatusAborted, "", nil)
	default:
		j.markRunning()
		m.publishStatus() // republish: activation moved a job from queued to running
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
	// the row belongs to the job, not to a turn: this is the only place a running
	// job's row is cleared, so a nudge turn never makes a live job disappear.
	m.publishActivity(j.id, "", j.num)
	m.publishStatus()
	close(j.done) // release waiting pollers before onComplete so their claim is visible
	m.onComplete(j)
}

// acquireSlot blocks until a slot is granted or the job is cancelled. A grant
// that raced our cancel still returns true: the caller holds the slot and releases it.
func (m *Manager) acquireSlot(j *job) bool {
	select {
	case <-j.grant:
		return true
	case <-j.ctx.Done():
		m.mu.Lock()
		defer m.mu.Unlock()
		if j.granted {
			return true
		}
		// still queued: leave the queue so no later dispatch grants a dead job
		m.waiters = bulk.SliceFilterInPlace(func(w *job) bool { return w != j }, m.waiters)
		return false
	}
}

// dispatchLocked hands free slots to waiters in job-number order, so a fan-out
// activates as sub-1, sub-2, ... however the start goroutines raced. Caller holds mu.
func (m *Manager) dispatchLocked() {
	m.waiters = bulk.SliceFilterInPlace(func(j *job) bool { return j.ctx.Err() == nil }, m.waiters)
	slices.SortFunc(m.waiters, func(a, b *job) int { return cmp.Compare(a.num, b.num) })
	for len(m.waiters) > 0 && m.running < m.opts.MaxConcurrent {
		j := m.waiters[0]
		m.waiters = slices.Delete(m.waiters, 0, 1)
		m.running++
		j.granted = true
		close(j.grant)
	}
}

// releaseSlot returns one concurrency slot and wakes the next waiter.
func (m *Manager) releaseSlot() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.running--
	m.dispatchLocked()
}

// poll blocks until id completes, PollTimeout elapses, or ctx is cancelled,
// returning false while still running so an interrupted turn releases the poll at once.
// agent_poll is ModeParallel, so a batch commits every tool header at dispatch and
// each payload only as its own job finishes: a batched result lands detached from
// the header naming it and has to say which sub-agent it came from. The third
// result reports whether this call shared its window with another.
func (m *Manager) poll(ctx context.Context, id string) (Job, bool, bool) {
	m.enterPoll()
	snap, complete := m.wait(ctx, id)
	return snap, complete, m.leavePoll()
}

// enterPoll registers a poll, marking the group batched once two overlap.
func (m *Manager) enterPoll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.polling++
	if m.polling > 1 { // every poll in an overlapping group needs its result named
		m.batched = true
	}
}

// leavePoll deregisters a poll and reports whether it overlapped another. The mark
// clears once the group empties, so a later lone poll is named nothing.
func (m *Manager) leavePoll() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.polling--
	batched := m.batched
	if m.polling == 0 {
		m.batched = false
	}
	return batched
}

// wait blocks on one job until completion, its poll timeout or a cancelled ctx.
// complete reports whether snap holds the claimed terminal result. A running
// job returns just its latest snapshot, a cancelled ctx an empty unclaimed Job.
func (m *Manager) wait(ctx context.Context, id string) (Job, bool) {
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
		// onComplete skips the enqueue while a poll is registered, so the last
		// poller to leave empty-handed has to re-arm delivery itself.
		orphan := j.pollers == 0 && !j.consumed
		j.mu.Unlock()
		if orphan {
			m.onComplete(j)
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-j.done:
		return m.claimResult(j), true
	case <-timer.C:
		// finish sets a final status before close(done), so checking the status
		// covers both a landed and an in-flight completion; the timer must claim
		if j.terminal() {
			return m.claimResult(j), true
		}
		return j.snapshot(), false // still running, caller reads progress for the payload
	case <-ctx.Done():
		// an interrupted turn discards the tool result, so claiming here would lose
		// the summary and defer to the orphan check that re-arms delivery instead.
		return Job{}, false
	}
}

// claimResult marks a job's result as delivered by this poll and snapshots it, so
// no later steer repeats it and no later notice re-names it.
func (m *Manager) claimResult(j *job) Job {
	j.mu.Lock()
	j.consumed = true
	j.mu.Unlock()
	// mark delivered by this poll and drop the id from every delivery list, so no
	// later steer repeats it and no later notice re-names it
	one := []string{j.id}
	m.mu.Lock()
	m.pending = dropIDs(m.pending, one)
	m.inFlight = dropIDs(m.inFlight, one)
	m.noticeBatch = dropIDs(m.noticeBatch, one)
	m.mu.Unlock()
	return j.snapshot()
}

// List returns a snapshot of every job in job-number order (sub-1, ..., sub-10).
func (m *Manager) List() []Job {
	m.mu.Lock()
	ids := bulk.MapKeysSlice(m.jobs)
	slices.SortFunc(ids, byJobNumber)
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
// aborted, and a finished one returns an error.
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

	var n int // only in-flight jobs are actually cancelled, finished ones are not counted
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
// context as one batched message. Called from a turn-start observer so pending
// completions reach the model on the next real turn without ever starting one.
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
	// cancel first: any late-starting or still-registered job dies with the manager
	// regardless of where StopAll's snapshot lands
	m.cancel()
	m.StopAll()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	timer := time.NewTimer(m.closeWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C: // jobs may be stuck on a slow provider, do not block shutdown
	}
	m.mu.Lock()
	rows := make([]activityKey, 0, len(m.jobs))
	for _, j := range m.jobs {
		rows = append(rows, activityKey{id: j.id, num: j.num})
	}
	m.pending = nil
	m.inFlight = nil
	m.noticeBatch = nil
	m.mu.Unlock()

	// the flag and the clears share pubMu: a publish already in flight lands
	// first, every later one sees closed, so these clears are the final UI writes
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	m.closed = true
	if fn := m.opts.Activity; fn != nil {
		for _, r := range rows {
			fn(r.id, "", r.num)
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

// onComplete queues a finished job's completion for the user and the model,
// unless a poll is waiting or already consumed it. Aborts are silent. Delivery
// is batched at the next step boundary (Boundary) or turn start (Flush).
func (m *Manager) onComplete(j *job) {
	if !j.finished() { // a poll left while the job is still running, nothing to deliver
		return
	}
	j.mu.Lock()
	status := j.status
	pollers := j.pollers // a registered poller carries the result, a steer would waste tokens
	consumed := j.consumed
	id := j.id
	j.mu.Unlock()

	if status == StatusAborted || pollers > 0 || consumed {
		return
	}
	m.enqueue(id)
}

// enqueue parks a completed id and fires the keyed UI notice naming every
// completion since the last delivered steer, so the collapsed line reads as one
// batch instead of one row per agent.
func (m *Manager) enqueue(id string) {
	// pubMu spans the snapshot and callback: near-simultaneous completions must
	// publish in append order, or a stale smaller batch lands after the full one
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	if m.closed { // a completion racing past Close: the UI is already cleared
		return
	}

	m.mu.Lock()
	if !slices.Contains(m.pending, id) {
		m.pending = append(m.pending, id)
	}
	if !slices.Contains(m.noticeBatch, id) { // a re-armed delivery must not double-name it
		m.noticeBatch = append(m.noticeBatch, id)
		slices.SortFunc(m.noticeBatch, byJobNumber)
	}
	batch := slices.Clone(m.noticeBatch)
	m.mu.Unlock()
	text := "Sub-agents " + strings.Join(batch, ", ") + " completed" // names the batch for the keyed UI notice
	if len(batch) == 1 {
		text = "Sub-agent " + batch[0] + " completed"
	}
	m.opts.Notice(text)
}

// Boundary returns one batched completion input for the step boundary, or nil.
// The host chains it into agent.Options.OnBoundary, so it runs on the loop
// goroutine at the exact moment the message lands: ids a poll claimed since
// completing are dropped and never named, because the poll response already
// carries their result.
func (m *Manager) Boundary() []agent.Input {
	m.mu.Lock()
	ids := slices.Clone(m.pending)
	m.mu.Unlock()

	deliverable := m.take(ids)
	if len(deliverable) == 0 {
		return nil
	}
	return []agent.Input{m.noticeInput(deliverable)}
}

// offer steers a batch into the running parent turn through the front-end
// Deliver hook. A false return (parent idle) unmarks the batch and leaves the
// ids pending for the next Flush.
func (m *Manager) offer(ids []string) {
	deliverable := m.take(ids)
	if len(deliverable) == 0 {
		return
	}
	in := m.noticeInput(deliverable)
	if !m.opts.Deliver(in) {
		m.mu.Lock()
		m.inFlight = dropIDs(m.inFlight, deliverable)
		m.mu.Unlock()
	}
}

// take filters ids down to those still needing delivery, excluding ones claimed
// by a poll or already carried by a queued input, then marks them in flight.
func (m *Manager) take(ids []string) []string {
	deliverable := make([]string, 0, len(ids))
	m.mu.Lock()
	for _, id := range ids {
		if slices.Contains(m.inFlight, id) {
			continue
		}
		j, ok := m.jobs[id]
		if !ok {
			continue
		}
		j.mu.Lock()
		free := j.pollers == 0 && !j.consumed // a poller carries the result itself
		j.mu.Unlock()
		if !free {
			continue
		}
		deliverable = append(deliverable, id)
		m.inFlight = append(m.inFlight, id)
		if !slices.Contains(m.pending, id) {
			m.pending = append(m.pending, id)
		}
	}
	m.mu.Unlock()
	slices.SortFunc(deliverable, byJobNumber)
	return deliverable
}

// noticeInput builds the batched completion steer. Delivered clears exactly the
// names the message carried. A name dropped by an interrupt stays pending, and it
// starts a fresh UI notice batch.
func (m *Manager) noticeInput(ids []string) agent.Input {
	notice := completionNotice(ids)
	return agent.Input{
		Text:     notice,
		Echo:     notice, // live echo where the notice lands
		Injected: true,   // a system notice rather than a typed prompt, excluded from recall
		Delivered: func() {
			m.mu.Lock()
			m.pending = dropIDs(m.pending, ids)
			m.inFlight = dropIDs(m.inFlight, ids)
			m.noticeBatch = nil
			m.mu.Unlock()
		},
	}
}

// byJobNumber orders sub-N ids numerically, so batches read sub-1, sub-2, sub-10.
// Unparseable ids sort first. The digits are read after the sub- prefix.
func byJobNumber(a, b string) int {
	na, _ := strconv.Atoi(strings.TrimPrefix(a, "sub-"))
	nb, _ := strconv.Atoi(strings.TrimPrefix(b, "sub-"))
	return cmp.Compare(na, nb)
}

// activityKey pairs a job's row key with its rank, so Close can clear every row
// without holding the manager lock across the UI hook.
type activityKey struct {
	id  string
	num int
}

// dropIDs removes every named id from ids, in place.
func dropIDs(ids, drop []string) []string {
	return bulk.SliceFilterInPlace(func(id string) bool { return !slices.Contains(drop, id) }, ids)
}

// Interrupted reports that the parent turn was interrupted: queued completion
// steers were dropped without their Delivered firing, so the in-flight marks
// must release or every later batch waits behind a delivery that never comes.
// The ids stay pending for the next Flush.
func (m *Manager) Interrupted() {
	m.mu.Lock()
	m.inFlight = nil
	m.mu.Unlock()
}

// completionNotice is the steer text naming completed sub-agents. The [tool]
// prefix marks generated context, so the model never reads it as operator speech.
func completionNotice(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		id := ids[0]
		return "[tool] Sub-agent " + id + " completed. Call agent_poll with id " + id + " to retrieve the summary."
	default:
		return fmt.Sprintf("[tool] Sub-agents %s completed. Poll each for its summary.", strings.Join(ids, ", "))
	}
}

// publishActivity pushes one activity row (empty text clears it), dropped once
// Close has cleared the UI so a straggler cannot repaint a row.
func (m *Manager) publishActivity(key, text string, rank int) {
	fn := m.opts.Activity
	if fn == nil {
		return
	}
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	if m.closed {
		return
	}
	fn(key, text, rank)
}

// publishStatus recomputes and pushes the status segment, empty clears it.
func (m *Manager) publishStatus() {
	fn := m.opts.Status
	if fn == nil {
		return
	}
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	if m.closed { // a straggler finishing past Close's wait must not re-add the segment
		return
	}
	m.mu.Lock()
	var running, queued, done int
	var oldest time.Duration // oldest live job's full age: queue wait plus active runtime
	for _, j := range m.jobs {
		s, started := j.runningSince()
		switch s {
		case StatusQueued:
			queued++
		case StatusRunning:
			running++
		case StatusDone:
			done++
			continue // terminal, not live: its stale age must never feed the note
		default:
			continue // aborted or errored: no longer shown at all
		}
		if d := time.Since(started); d > oldest { // started is stamped for every live job
			oldest = d
		}
	}
	m.mu.Unlock()

	if running == 0 && queued == 0 && done == 0 {
		fn("", "")
		return
	}
	parts := make([]string, 0, 4)
	if running > 0 {
		s := fmt.Sprintf("%d running", running)
		if oldest > 0 { // age note only while something is actively running
			s += " (oldest " + strutil.Elapsed(oldest) + ")"
		}
		parts = append(parts, s)
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", queued))
	}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d done", done))
	}
	var short string
	if live := running + queued; live > 0 { // the short segment counts every live job
		short = fmt.Sprintf("sub %d", live)
	}
	fn("subagents: "+strings.Join(parts, ", "), short)
}

// pollProgress reads a still-pending job's wait or active runtime and child context
// usage for the timeout payload.
func (j *job) pollProgress() string {
	j.mu.Lock()
	status := j.status
	id := j.id
	var elapsed time.Duration
	switch status {
	case StatusQueued: // never activated, waiting on the semaphore
		elapsed = queueWait(j.started, j.ended)
	default: // running; active runtime since it took a slot
		elapsed = runTime(j.activated, j.ended)
	}
	j.mu.Unlock()

	verb := "still running after"
	if status == StatusQueued {
		verb = "queued for"
	}
	c := j.tokens.Context()
	used, win := c.Used, c.Window
	s := fmt.Sprintf("sub-agent %s %s %s", id, verb, strutil.Elapsed(elapsed))
	switch {
	case win > 0:
		return s + fmt.Sprintf(", context %d/%d tokens used", used, win)
	case used > 0:
		return s + fmt.Sprintf(", about %d context tokens used", used)
	default:
		return s
	}
}

// shortLabel reduces task text to one rune-safe line.
func shortLabel(text string) string {
	line := strings.TrimSpace(strutil.FirstLine(text))
	if len([]rune(line)) <= maxLabelLen {
		return line
	}
	return strutil.Clip(line, maxLabelLen)
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
