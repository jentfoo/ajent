package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBashBgSegment(t *testing.T) {
	t.Parallel()

	// zero live runs removes the segment
	t.Run("zero_removes", func(t *testing.T) {
		seg := bashBgSegment(0)
		assert.Empty(t, seg.Text)
		assert.Empty(t, seg.Short)
	})

	t.Run("counts_live_runs", func(t *testing.T) {
		seg := bashBgSegment(2)
		assert.Equal(t, "bash bg 2", seg.Text)
		assert.Equal(t, "bg 2", seg.Short)
	})

	t.Run("placement_next_to_subagents", func(t *testing.T) {
		seg := bashBgSegment(1)
		assert.Equal(t, segBashBg, seg.Key)
		assert.Greater(t, seg.Order, orderSubagents)
		assert.Less(t, seg.Order, orderPlan)
	})
}
