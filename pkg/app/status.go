package app

import (
	"github.com/jentfoo/ajent/pkg/tui"
)

// Status segment keys, in the order they appear after the model. The numbers leave gaps so
// a new segment can be inserted between two existing ones.
const (
	segReasoning   = "reasoning"
	segMCP         = "mcp"
	segSubagents   = "subagents"
	segPlan        = "plan"
	segPermissions = "permissions"
	segHint        = "hint"

	orderReasoning   = 20
	orderMCP         = 30
	orderSubagents   = 40
	orderPlan        = 50
	orderPermissions = 60
	orderHint        = 70
)

// segSpec is a segment's display position and its step in the collapse ladder, where 0
// gives up its full text first, ascending numbers follow, and a negative never collapses.
type segSpec struct{ order, priority int }

// segSpecs is the one place a key's placement is declared, so a publisher that clears and
// re-shares cannot land in a different slot than it left.
var segSpecs = map[string]segSpec{
	segReasoning:   {orderReasoning, 0}, // re-derivable elsewhere: yields its full label first
	segMCP:         {orderMCP, 5},
	segSubagents:   {orderSubagents, 4},
	segPlan:        {orderPlan, 2},
	segPermissions: {orderPermissions, 8}, // safety state: keeps its full label late
	segHint:        {orderHint, -1},       // imperative text with no useful short form
}

// segment builds a keyed status segment carrying the placement declared for that key. An
// empty Text removes the segment.
func segment(key, text, short string) tui.Segment {
	spec := segSpecs[key]
	return tui.Segment{Key: key, Text: text, Short: short, Order: spec.order, Priority: spec.priority}
}
