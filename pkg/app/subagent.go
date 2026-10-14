package app

import (
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tools"
)

// childToolSource adapts the parent registry to subagent.ToolSource. A child
// runs raw tools with no guard chain, so every tool is wrapped to fit result
// image blocks, the same rewrite the guard wrapper applies in the parent.
type childToolSource struct{ reg *tools.Registry }

var _ subagent.ToolSource = childToolSource{}

// All returns every registered tool wrapped to fit result image blocks.
func (s childToolSource) All() []agent.Tool {
	src := s.reg.All()
	out := make([]agent.Tool, len(src))
	for i, t := range src {
		out[i] = tools.NormalizeImages(t)
	}
	return out
}

func (s childToolSource) ReadOnly(name string) bool {
	return s.reg.ReadOnly(name)
}
