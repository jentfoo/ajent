package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestQuitArmFire pins the shared ctrl+c gesture: one press arms and holds the
// hint line, a second inside the window commits with the committed label, and
// a press after the window closed arms anew. Labels name the surface, so the
// host loop and attach render the same behavior.
func TestQuitArmFire(t *testing.T) {
	t.Parallel()

	t.Run("second_press_commits", func(t *testing.T) {
		var painted []string
		q := quitArm{hints: newHintBoard(func(text, short string) { painted = append(painted, text) }),
			again: "ctrl+c again to quit", going: "quitting…"}
		assert.False(t, q.fire())
		assert.True(t, q.fire())
		assert.Equal(t, []string{"ctrl+c again to quit", "quitting…"}, painted)
	})

	t.Run("labels_name_the_surface", func(t *testing.T) {
		var painted []string
		q := quitArm{hints: newHintBoard(func(text, short string) { painted = append(painted, text) }),
			again: "ctrl+c again to detach", going: "detaching…"}
		assert.False(t, q.fire())
		assert.True(t, q.fire())
		assert.Equal(t, []string{"ctrl+c again to detach", "detaching…"}, painted)
	})

	t.Run("expired_window_re_arms", func(t *testing.T) {
		var painted []string
		q := quitArm{hints: newHintBoard(func(text, short string) { painted = append(painted, text) }),
			again: "again", going: "going"}
		q.at = time.Now().Add(-2 * doublePressWindow)
		assert.False(t, q.fire()) // the window closed: this press arms anew
		assert.Equal(t, []string{"again"}, painted)
	})
}

// TestInterruptNotice pins the one mapping both loops show for a keypress that
// stopped real work: survey and shell name their cancellations, a stopped turn
// stays silent because its own stream is the news.
func TestInterruptNotice(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "cancelled project survey", interruptNotice(stopSurvey.String()))
	assert.Equal(t, "cancelled shell command", interruptNotice(stopShell.String()))
	assert.Empty(t, interruptNotice(stopTurn.String()))
	assert.Empty(t, interruptNotice(stopNothing.String()))
}
