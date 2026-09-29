package subagent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestJobElapsed(t *testing.T) {
	t.Parallel()

	const wait, run = 30 * time.Second, 2 * time.Minute
	started := time.Unix(1000, 0)
	cases := []struct {
		name      string
		status    Status
		activated bool // set Activated to started+wait
		want      time.Duration
	}{
		{"done_reports_active", StatusDone, true, run},
		{"error_reports_active", StatusError, true, run},
		{"aborted_after_run_reports_active", StatusAborted, true, run},
		{"aborted_before_run_shows_wait", StatusAborted, false, wait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := Job{
				Status:  tc.status,
				Started: started,
				Ended:   started.Add(wait + run),
			}
			if tc.activated {
				j.Activated = started.Add(wait)
			} else { // aborted before running froze at the wait mark
				j.Ended = started.Add(wait)
			}
			assert.Equal(t, tc.want, j.Elapsed())
		})
	}

	t.Run("queued_reports_live_wait", func(t *testing.T) {
		j := Job{Status: StatusQueued, Started: time.Now().Add(-wait)}
		assert.InDelta(t, wait.Seconds(), j.Elapsed().Seconds(), 1)
	})

	t.Run("running_reports_live_active", func(t *testing.T) {
		j := Job{Status: StatusRunning, Activated: time.Now().Add(-run)}
		assert.InDelta(t, run.Seconds(), j.Elapsed().Seconds(), 1)
	})
}
