package command

import (
	"context"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// filterPlaceholder is the default picker hint for narrowing a list.
const filterPlaceholder = "filter"

// toolsCommand reshapes the enabled set. Before this run's first dispatched
// prompt the picker lists every registered tool and the selection is free (a
// resumed session preselects its recorded set). After it only disabled tools
// are offered, since the tool block the model has already seen is not retractable.
func toolsCommand(ctx context.Context, _ string, c Console) error {
	reg := c.Tools()
	if reg == nil {
		c.Notify("no tools registered", levelWarn)
		return nil
	}
	if !c.Dispatched() {
		return toolsFreeSelect(ctx, c, reg)
	}
	return toolsWidenOnly(ctx, c, reg)
}

// ApplyToolSet reshapes the enabled set without a dialog, the path the
// control socket and the pickers below share: a free replacement before this
// run's first dispatched prompt, enable-only after, since the tool block the
// model has already seen is not retractable. Recording, the tools event and
// the cache-miss notice ride ToolsChanged, so a reshape reaching every
// surface stays one code path. A set that changes nothing records nothing.
func ApplyToolSet(c Console, names []string) {
	reg := c.Tools()
	if reg == nil {
		return
	}
	before := reg.Names()
	if c.Dispatched() {
		reg.Enable(names)
	} else {
		reg.SetEnabled(names)
	}
	if slices.Equal(reg.Names(), before) {
		return // nothing moved: no override, no event, no cache-miss notice
	}
	c.ToolsChanged()
	c.Notify("tool block changed; prompt cache will miss", levelInfo)
}

// toolsFreeSelect lists every tool with the current set preselected.
// The selection is free to enable or disable anything ahead of the first prompt.
func toolsFreeSelect(ctx context.Context, c Console, reg *tools.Registry) error {
	all := reg.All()
	rows := reg.Units(all)
	items, initial := toolRows(reg, rows, c)
	picked, err := c.MultiPick(ctx, "Tools", items,
		tui.MultiPickOptions{Placeholder: filterPlaceholder, Initial: initial})
	if err != nil {
		return nil // cancelled
	}
	names := make([]string, 0, len(all))
	for _, p := range picked {
		names = append(names, rows[p].Names...) // a group row expands to every member
	}
	ApplyToolSet(c, names)
	return nil
}

// toolsWidenOnly lists only disabled tools, and selecting enables them. Nothing can
// be turned off again for the rest of the session.
func toolsWidenOnly(ctx context.Context, c Console, reg *tools.Registry) error {
	disabled := reg.Disabled()
	if len(disabled) == 0 {
		c.Notify("all tools already enabled", levelInfo)
		return nil
	}
	rows := reg.Units(disabled)
	items, _ := toolRows(reg, rows, c)
	picked, err := c.MultiPick(ctx, "Enable tools", items,
		tui.MultiPickOptions{Placeholder: filterPlaceholder})
	if err != nil {
		return nil // cancelled
	}
	if len(picked) == 0 {
		return nil
	}
	toEnable := make([]string, 0, len(disabled))
	for _, p := range picked {
		toEnable = append(toEnable, rows[p].Names...) // a group row expands to every member
	}
	ApplyToolSet(c, toEnable)
	return nil
}

// toolRows groups offered rows by source for MultiPick headers, returning the
// items plus which indexes are already fully enabled. MCP sources render an
// enhanced header (tool count, startup mode, connection state) from the manager.
func toolRows(reg *tools.Registry, rows []tools.Row, c Console) ([]tui.PickItem, []int) {
	enabled := bulk.SliceToSet(reg.Names())
	var groups []MCPGroup
	if mgr := c.MCP(); mgr != nil {
		groups = mgr.Groups()
	}
	labels := make(map[string]string, len(groups)) // source-to-header-label for MCP servers
	for _, g := range groups {
		labels[g.Source] = g.Label
	}

	// group by source so MultiPick emits a header when the group changes, stable
	// sort keeping declaration order within a source (builtins up front, ahead of MCP)
	slices.SortStableFunc(rows, func(a, b tools.Row) int { return strings.Compare(a.Source, b.Source) })
	items := make([]tui.PickItem, len(rows))
	var initial []int
	for i, rw := range rows {
		src := rw.Source
		label := src
		if l, ok := labels[src]; ok { // MCP servers carry an enhanced header
			label = l
		}
		items[i] = tui.PickItem{Label: rw.Name, Group: label}
		allOn := tools.AllPresent(rw.Names, enabled)
		if allOn {
			initial = append(initial, i)
		}
	}
	return items, initial
}
