package refs

import (
	"github.com/jentfoo/ajent/pkg/agent"
)

// Normalize expands @ references in one steered or follow-up input the way the
// pump expands a fresh prompt. Notices surface through warn. Inputs with
// assembled context (Before, Blocks or After) pass through unexpanded; a nil
// expander passes anything through.
func Normalize(x *Expander, in agent.Input, warn func(string)) agent.Input {
	if x == nil {
		return in
	}
	if in.Text == "" || len(in.Before) > 0 || len(in.Blocks) > 0 || in.After != nil {
		return in
	}
	res := x.Expand(in.Text)
	for _, n := range res.Notices {
		warn(n)
	}
	in.Text = res.Text
	in.After = res.Run
	in.Prepared = true
	return in
}
