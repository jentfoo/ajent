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
