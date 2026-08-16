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
	ID      string
	Status  Status
	Task    string // shortened task label, single line
	Started time.Time
	Ended   time.Time
	Summary string
	Err     error
}

// job is the live state behind a public Job. Fields under mu are read by Pollers
// while the owning goroutine mutates them.
type job struct {
	id           string // sub-1, sub-2, …
	task         string // full task text for the prompt
	label        string // shortened single-line label for rows and /agents
	instructions string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}      // closed when the owning goroutine finishes
	tokens *tokens.Accounting // child ledger, created at Start for poll payloads

	mu       sync.Mutex
	status   Status
	started  time.Time
	ended    time.Time
	summary  string
	err      error
	pollers  int
	consumed bool // result delivery handled (by a poll or a steer); suppresses later offers
}

// snapshot copies the public fields under lock.
func (j *job) snapshot() Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Job{
		ID:      j.id,
		Status:  j.status,
		Task:    j.label,
		Started: j.started,
		Ended:   j.ended,
		Summary: j.summary,
		Err:     j.err,
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

// markRunning flips a queued job to running. started keeps its Start stamp so
// elapsed includes time spent waiting on the semaphore.
func (j *job) markRunning() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == StatusQueued {
		j.status = StatusRunning
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
