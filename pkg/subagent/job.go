package subagent

import (
	"context"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/tokens"
)

// Status is a job's lifecycle state.
type Status uint8

const (
	StatusQueued Status = iota // waiting on the concurrency semaphore
	StatusRunning
	StatusDone
	StatusError
	StatusAborted
)

var statusNames = map[Status]string{
	StatusQueued:  "queued",
	StatusRunning: "running",
	StatusDone:    "done",
	StatusError:   "error",
	StatusAborted: "aborted",
}

// String returns the canonical status name used in /agents and tool output.
func (s Status) String() string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return "unknown"
}

// Job is a public snapshot of one investigation, for List and Poll callers.
type Job struct {
	ID        string
	Status    Status
	Task      string // shortened task label, single line
	Started   time.Time
	Activated time.Time
	Ended     time.Time
	Summary   string
	Err       error
}

// Elapsed returns the duration list and /agents should show for this job's status.
// A job that never activated (still queued, or aborted before it ran) reports its
// queue wait; anything else reports active runtime, frozen once finished.
func (j Job) Elapsed() time.Duration {
	if j.Activated.IsZero() {
		return queueWait(j.Started, j.Ended)
	}
	return runTime(j.Activated, j.Ended)
}

// queueWait is submission until finish for a job that never activated; it keeps
// counting while the job still waits on the semaphore.
func queueWait(started, ended time.Time) time.Duration {
	if !ended.IsZero() {
		return ended.Sub(started)
	}
	return time.Since(started)
}

// runTime is active runtime from activation until finish (or now), zero for a job
// that never activated.
func runTime(activated, ended time.Time) time.Duration {
	if !ended.IsZero() {
		return ended.Sub(activated)
	}
	return time.Since(activated)
}

// job is the live state behind a public Job. Fields under mu are read by Pollers
// while the owning goroutine mutates them.
type job struct {
	id           string // sub-1, sub-2, ...
	num          int    // id's number, the stable rank of the job's activity row
	task         string // full task text for the prompt
	label        string // shortened single-line label for rows and /agents
	instructions string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}      // closed when the owning goroutine finishes
	tokens *tokens.Accounting // child ledger, created at Start for poll payloads

	mu        sync.Mutex
	status    Status
	started   time.Time // submission (queued) stamp
	activated time.Time // first slot acquisition, set in markRunning
	ended     time.Time
	summary   string
	err       error
	pollers   int
	consumed  bool // result delivery handled (by a poll or a steer), suppresses later offers
}

// snapshot copies the public fields under lock.
func (j *job) snapshot() Job {
	j.mu.Lock()
	defer j.mu.Unlock()

	return Job{
		ID:        j.id,
		Status:    j.status,
		Task:      j.label,
		Started:   j.started,
		Activated: j.activated,
		Ended:     j.ended,
		Summary:   j.summary,
		Err:       j.err,
	}
}

// finish records the terminal status, timestamps and result.
func (j *job) finish(s Status, summary string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.status = s
	if j.ended.IsZero() {
		j.ended = time.Now()
	}
	j.summary = summary
	j.err = err
}

// markRunning flips a queued job to running, stamping when it took its slot so
// queue wait and active runtime stay separate.
func (j *job) markRunning() {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.status == StatusQueued {
		j.status = StatusRunning
		j.activated = time.Now()
	}
}

// setQueued marks a freshly spawned job as waiting on the semaphore.
func (j *job) setQueued(now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.status = StatusQueued
	if j.started.IsZero() {
		j.started = now
	}
}

// status returns the current status under lock.
func (j *job) statusOf() Status {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.status
}

// runningSince reports the live status and started stamp under one lock, so a
// caller that needs both never reads them at different instants.
func (j *job) runningSince() (Status, time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.status, j.started
}

// finished reports whether the owning goroutine has completed, without blocking.
func (j *job) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// terminal reports whether a final status is recorded. finish sets it before
// close(j.done), so the timeout branch checks it to avoid reporting running.
func (j *job) terminal() bool {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.status == StatusDone || j.status == StatusError || j.status == StatusAborted
}
