package command

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// usageCommand renders the session's token ledger as markdown through Console.Print.
func usageCommand(_ context.Context, _ string, c Console) error {
	st := c.State()
	if st == nil || st.Tokens == nil {
		c.Notify("no accounting available", levelWarn)
		return nil
	}
	t := st.Tokens

	var b strings.Builder
	b.WriteString("# Usage\n")

	cs := t.Context()
	// prefer the state's declared window; fall back to what accounting reports
	window := st.Model.ContextWindow
	if window <= 0 {
		window = cs.Window
	}
	budget := cs.Budget()

	// fill against the compaction threshold when one is configured, so the number
	// matches the status bar; fall back to the response-safe budget otherwise.
	denom := cs.Compact
	if denom <= 0 {
		denom = budget
	}
	fmt.Fprintf(&b, "\ncontext: %s / %s  (%d%% of %s before compaction)\n",
		strutil.FormatTokens(cs.Used), strutil.FormatTokens(window),
		pctOf(cs.Used, denom), strutil.FormatTokens(denom))
	if cs.Reserve > 0 {
		fmt.Fprintf(&b, "\nreserve: %s held back for the response\n",
			strutil.FormatTokens(cs.Reserve))
	}

	b.WriteString("\n| turns | input | output |\n")
	b.WriteString("|------:|------:|-------:|")

	total := t.Total()
	n := t.TurnsCount()
	est := t.EstimatedTurns()

	fmt.Fprintf(&b, "| %d | %s | %s |\n", n,
		strutil.FormatTokens(total.Input), strutil.FormatTokens(total.Output))

	// delegation cost is tracked separately so /usage shows what sub-agents spent.
	if child := t.ChildTotal(); child.Input > 0 || child.Output > 0 {
		fmt.Fprintf(&b, "\nof which sub-agents: %s in / %s out\n",
			strutil.FormatTokens(child.Input), strutil.FormatTokens(child.Output))
	}

	if byModel := t.ByModel(); len(byModel) > 1 {
		b.WriteString("\n| model | input | output |\n")
		b.WriteString("|-------|------:|-------:|\n")
		keys := bulk.MapKeysSlice(byModel)
		slices.Sort(keys)
		for _, key := range keys {
			m := byModel[key]
			fmt.Fprintf(&b, "| %s | %s | %s |\n", key,
				strutil.FormatTokens(m.Input), strutil.FormatTokens(m.Output))
		}
	}

	if est > 0 {
		b.WriteString("\n_footnote: some turns reported no usage and are estimated._\n")
	}

	c.Print(b.String())
	return nil
}

// pctOf clamps the percentage of used against a denominator to [0,100], matching
// how the status bar fills.
func pctOf(used, total int) int {
	if total <= 0 || used < 1 {
		return 0
	}
	p := used * 100 / total
	if p > 100 {
		p = 100
	}
	return p
}
