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
	t := st.Tokens

	var b strings.Builder
	b.WriteString("# Usage\n")

	cs := t.Context()
	// prefer the state's declared window, falling back to what accounting reports
	window := st.Model.ContextWindow
	if window <= 0 {
		window = cs.Window
	}
	budget := max(cs.Window-cs.Reserve, 0) // usable input, floored against a bad reserve

	// fill against the compaction threshold when one is configured, so the number
	// matches the status bar, falling back to the response-safe budget otherwise.
	denom := cs.Compact
	if denom <= 0 {
		denom = budget
	}
	pct := 0 // clamped to [0,100], matching how the status bar fills
	if denom > 0 && cs.Used >= 1 {
		pct = min(cs.Used*100/denom, 100)
	}
	_, _ = fmt.Fprintf(&b, "\ncontext: %s / %s  (%d%% of %s before compaction)\n",
		strutil.FormatTokens(cs.Used), strutil.FormatTokens(window),
		pct, strutil.FormatTokens(denom))
	if cs.Reserve > 0 {
		_, _ = fmt.Fprintf(&b, "\nreserve: %s held back for the response\n",
			strutil.FormatTokens(cs.Reserve))
	}

	b.WriteString("\n| turns | input | output |\n")
	b.WriteString("|------:|------:|-------:|\n")

	total := t.Total()
	n := t.TurnsCount()
	est := t.EstimatedTurns()

	_, _ = fmt.Fprintf(&b, "| %d | %s | %s |\n", n,
		strutil.FormatTokens(total.Input), strutil.FormatTokens(total.Output))

	// delegation cost is tracked separately so /usage shows what sub-agents spent.
	if child := t.ChildTotal(); child.Input > 0 || child.Output > 0 {
		_, _ = fmt.Fprintf(&b, "\nof which sub-agents: %s in / %s out\n",
			strutil.FormatTokens(child.Input), strutil.FormatTokens(child.Output))
	}

	if byModel := t.ByModel(); len(byModel) > 1 {
		b.WriteString("\n| model | input | output |\n")
		b.WriteString("|-------|------:|-------:|\n")
		keys := bulk.MapKeysSlice(byModel)
		slices.Sort(keys)
		for _, key := range keys {
			m := byModel[key]
			_, _ = fmt.Fprintf(&b, "| %s | %s | %s |\n", key,
				strutil.FormatTokens(m.Input), strutil.FormatTokens(m.Output))
		}
	}

	if est > 0 {
		b.WriteString("\n_footnote: some turns reported no usage and are estimated._\n")
	}

	c.Print(b.String())
	return nil
}
