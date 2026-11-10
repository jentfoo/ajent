package app

import (
	"fmt"

	"github.com/jentfoo/ajent/pkg/tui"
)

// Status segment keys, in the order they appear after the model. The numbers leave gaps so
// a new segment can be inserted between two existing ones.
const (
	segReasoning   = "reasoning"
	segMCP         = "mcp"
	segSubagents   = "subagents"
	segBashBg      = "bashbg"
	segPlan        = "plan"
	segPermissions = "permissions"
	segHint        = "hint"

	orderReasoning   = 20
	orderMCP         = 30
	orderSubagents   = 40
	orderBashBg      = 45
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
	segBashBg:      {orderBashBg, 3}, // informational like subagents, count-only so it collapses early
	segPlan:        {orderPlan, 2},
	segPermissions: {orderPermissions, 8}, // safety state: keeps its full label late
	segHint:        {orderHint, -1},       // imperative text with no useful short form
}

// bashBgSegment builds the live background-bash segment from the run count;
// zero removes it. Count-only text: nothing repaints between events, so an age
// note would go stale.
func bashBgSegment(running int) tui.Segment {
	var text, short string
	if running > 0 {
		text = fmt.Sprintf("bash bg %d", running)
		short = fmt.Sprintf("bg %d", running)
	}
	return segment(segBashBg, text, short)
}

// segment builds a keyed status segment carrying the placement declared for that key. An
// empty Text removes the segment.
func segment(key, text, short string) tui.Segment {
	spec := segSpecs[key]
	return tui.Segment{Key: key, Text: text, Short: short, Order: spec.order, Priority: spec.priority}
}
