package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type customPayload struct {
	Phase string `json:"phase"`
	Round int    `json:"round"`
}

func customEntry(id, customType string, v any) Entry {
	return Entry{ID: id, Type: TypeCustom,
		Data: mustJSON(CustomData{CustomType: customType, Data: mustJSON(v)})}
}

func TestLatestCustom(t *testing.T) {
	t.Parallel()

	t.Run("newest_wins", func(t *testing.T) {
		branch := []Entry{
			customEntry("c1", "plan", customPayload{Phase: "planning", Round: 1}),
			msgWithID("m1", llmText("hi")),
			customEntry("c2", "plan", customPayload{Phase: "reviewing", Round: 3}),
		}
		var got customPayload
		require.True(t, LatestCustom(branch, "plan", &got))
		assert.Equal(t, customPayload{Phase: "reviewing", Round: 3}, got)
	})

	t.Run("ignores_other_types", func(t *testing.T) {
		branch := []Entry{
			customEntry("c1", "plan", customPayload{Phase: "planning"}),
			customEntry("c2", "other", customPayload{Phase: "nope"}),
		}
		var got customPayload
		require.True(t, LatestCustom(branch, "plan", &got))
		assert.Equal(t, "planning", got.Phase)
	})

	t.Run("missing_reports_false", func(t *testing.T) {
		branch := []Entry{msgWithID("m1", llmText("hi"))}
		var got customPayload
		assert.False(t, LatestCustom(branch, "plan", &got))
	})

	t.Run("empty_payload_false", func(t *testing.T) {
		branch := []Entry{{ID: "c1", Type: TypeCustom,
			Data: mustJSON(CustomData{CustomType: "plan"})}}
		var got customPayload
		assert.False(t, LatestCustom(branch, "plan", &got))
	})
}

func TestCustomLanded(t *testing.T) {
	t.Parallel()

	t.Run("message_after_custom_lands", func(t *testing.T) {
		branch := []Entry{
			customEntry("c1", "plan", customPayload{Phase: "implementing"}),
			msgWithID("m1", llmText("kickoff")),
		}
		assert.True(t, CustomLanded(branch, "plan"))
	})

	t.Run("custom_at_tail_never_landed", func(t *testing.T) {
		branch := []Entry{
			msgWithID("m1", llmText("earlier")),
			customEntry("c1", "plan", customPayload{Phase: "implementing"}),
		}
		assert.False(t, CustomLanded(branch, "plan"))
	})

	// bookkeeping entries after the custom entry are not delivery: only a
	// message proves the recorded phase began
	t.Run("non_message_entries_do_not_count", func(t *testing.T) {
		branch := []Entry{
			customEntry("c1", "plan", customPayload{Phase: "implementing"}),
			{ID: "mc1", Type: TypeModelChange},
		}
		assert.False(t, CustomLanded(branch, "plan"))
	})

	t.Run("other_custom_types_ignored", func(t *testing.T) {
		branch := []Entry{
			customEntry("c1", "plan", customPayload{Phase: "implementing"}),
			customEntry("c2", "other", customPayload{}),
		}
		assert.False(t, CustomLanded(branch, "plan"))
	})

	// no entry of the type means nothing can be pending, and the caller only
	// asks after a successful LatestCustom anyway
	t.Run("missing_custom_nothing_pending", func(t *testing.T) {
		assert.True(t, CustomLanded([]Entry{msgWithID("m1", llmText("hi"))}, "plan"))
	})

	// a branch of bookkeeping alone reaches the scan's end, which the godoc
	// counts as landed: no entry of the type, nothing pending
	t.Run("bookkeeping_only_lands", func(t *testing.T) {
		branch := []Entry{
			{ID: "s1", Type: TypeSession},
			{ID: "mc1", Type: TypeModelChange},
		}
		assert.True(t, CustomLanded(branch, "plan"))
	})
}
