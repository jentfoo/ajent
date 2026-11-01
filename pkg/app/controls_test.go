package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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
