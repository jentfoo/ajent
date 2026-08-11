package command

import (
	"context"
	"slices"

	"github.com/jentfoo/ajent/pkg/agent"
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
	items, initial := toolItems(reg, all)
	picked, err := c.MultiPick(context.Background(), "Tools", items,
		tui.MultiPickOptions{Placeholder: "filter", Initial: initial})
	if err != nil {
		return nil // cancelled
	}
	reg.SetEnabled(pickedNames(all, picked))
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
	items, _ := toolItems(reg, disabled)
	picked, err := c.MultiPick(context.Background(), "Enable tools", items,
		tui.MultiPickOptions{Placeholder: "filter"})
	if err != nil {
		return nil // cancelled
	}
	if len(picked) == 0 {
		return nil
	}
	toEnable := make([]string, 0, len(picked))
	for _, i := range picked {
		toEnable = append(toEnable, disabled[i].Name())
	}
	reg.Enable(toEnable)
	c.ToolsChanged()
	c.Notify("tool block changed; prompt cache will miss", levelInfo)
	return nil
}

// toolItems builds PickItems grouped by source, returning the items and the set
// of indexes that should start selected (the currently enabled tools present in
// the offered slice).
func toolItems(reg *tools.Registry, offered []agent.Tool) ([]tui.PickItem, []int) {
	enabled := sliceToSet(reg.Names())
	// group by source so MultiPick emits a header when the group changes
	slices.SortStableFunc(offered, func(a, b agent.Tool) int {
		sa, sb := reg.Source(a.Name()), reg.Source(b.Name())
		if sa == sb {
			return 0
		}
		if sa < sb {
			return -1
		}
		return 1
	})
	items := make([]tui.PickItem, len(offered))
	var initial []int
	for i, t := range offered {
		items[i] = tui.PickItem{
			Label: t.Name(),
			Group: reg.Source(t.Name()),
		}
		if _, ok := enabled[t.Name()]; ok {
			initial = append(initial, i)
		}
	}
	return items, initial
}

// pickedNames maps the chosen indexes back to tool names, preserving order.
func pickedNames(offered []agent.Tool, picked []int) []string {
	out := make([]string, 0, len(picked))
	for _, i := range picked {
		out = append(out, offered[i].Name())
	}
	return out
}

// sliceToSet builds a set for membership checks.
func sliceToSet(s []string) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for _, v := range s {
		out[v] = struct{}{}
	}
	return out
}
