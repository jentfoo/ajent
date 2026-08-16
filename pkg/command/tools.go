package command

import (
	"context"
	"slices"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// toolsCommand widens the enabled set. Before the first prompt the picker lists
// every registered tool and the selection is free; after it only disabled tools
// are offered, since the tool block the model has already seen is not retractable.
func toolsCommand(_ context.Context, _ string, c Console) error {
	reg := c.Tools()
	if reg == nil {
		c.Notify("no tools registered", levelWarn)
		return nil
	}
	if !c.Started() {
		return toolsFreeSelect(c, reg)
	}
	return toolsWidenOnly(c, reg)
}

// toolsFreeSelect lists every tool with the current set preselected; the
// selection is free to enable or disable anything ahead of the first prompt.
func toolsFreeSelect(c Console, reg *tools.Registry) error {
	all := reg.All()
	rows := reg.Units(all)
	items, initial := toolRows(reg, rows, c)
	picked, err := c.MultiPick(context.Background(), "Tools", items,
		tui.MultiPickOptions{Placeholder: "filter", Initial: initial})
	if err != nil {
		return nil // cancelled
	}
	var names []string
	for _, p := range picked {
		names = append(names, rows[p].Names...) // a group row expands to every member
	}
	reg.SetEnabled(names)
	c.ToolsChanged()
	c.Notify("tool block changed; prompt cache will miss", levelInfo)
	return nil
}

// toolsWidenOnly lists only disabled tools; selecting enables them. Nothing can
// be turned off again for the rest of the session.
func toolsWidenOnly(c Console, reg *tools.Registry) error {
	disabled := reg.Disabled()
	if len(disabled) == 0 {
		c.Notify("all tools already enabled", levelInfo)
		return nil
	}
	rows := reg.Units(disabled)
	items, _ := toolRows(reg, rows, c)
	picked, err := c.MultiPick(context.Background(), "Enable tools", items,
		tui.MultiPickOptions{Placeholder: "filter"})
	if err != nil {
		return nil // cancelled
	}
	if len(picked) == 0 {
		return nil
	}
	var toEnable []string
	for _, p := range picked {
		toEnable = append(toEnable, rows[p].Names...) // a group row expands to every member
	}
	reg.Enable(toEnable)
	c.ToolsChanged()
	c.Notify("tool block changed; prompt cache will miss", levelInfo)
	return nil
}

// toolRows groups offered rows by source for MultiPick headers, returning the
// items plus which indexes are already fully enabled. MCP sources render an
// enhanced header (tool count, startup mode, connection state) from the manager.
func toolRows(reg *tools.Registry, rows []tools.Row, c Console) ([]tui.PickItem, []int) {
	enabled := bulk.SliceToSet(reg.Names())
	labels := mcpGroupLabels(c)

	// group by source so MultiPick emits a header when the group changes; stable
	// sort keeps declaration order within a source (builtins up front, ahead of MCP)
	slices.SortStableFunc(rows, func(a, b tools.Row) int {
		sa, sb := a.Source, b.Source
		if sa == sb {
			return 0
		}
		if sa < sb {
			return -1
		}
		return 1
	})
	items := make([]tui.PickItem, len(rows))
	var initial []int
	for i, rw := range rows {
		src := rw.Source
		label := src
		if l, ok := labels[src]; ok { // MCP servers carry an enhanced header
			label = l
		}
		items[i] = tui.PickItem{Label: rw.Name, Group: label}
		allOn := true
		for _, n := range rw.Names {
			if _, ok := enabled[n]; !ok {
				allOn = false
				break
			}
		}
		if allOn {
			initial = append(initial, i)
		}
	}
	return items, initial
}

// mcpGroupLabels builds source→header-label for MCP servers from the manager.
func mcpGroupLabels(c Console) map[string]string {
	labels := make(map[string]string)
	if mgr := c.MCP(); mgr != nil {
		for _, g := range mgr.Groups() {
			labels[g.Source] = g.Label
		}
	}
	return labels
}
