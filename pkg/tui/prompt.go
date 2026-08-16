package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

const (
	selectMarker   = "> "
	selectIndent   = "  "
	interactionCap = 9 // number keys select directly up to this many options
)

// Option is one choice in a Select.
type Option struct {
	Label  string
	Detail string
}

// ItemMark colors an optional leading Tag on a picker row so roles read at a
// glance. Zero means no colored tag.
type ItemMark int

const (
	MarkNone      ItemMark = iota
	MarkUser               // blue "user" tag (rewind tree)
	MarkAssistant          // yellow "assistant" tag (rewind tree)
)

// PickItem is one row of a filterable list.
type PickItem struct {
	Label  string   // primary text
	Detail string   // dim trailing text
	Terms  []string // extra strings the filter matches, not displayed
	Group  string   // source label; a dim header is emitted when it changes
	Tag    string   // optional short role word rendered colored before Label
	Mark   ItemMark // colors Tag; MarkNone leaves Tag uncolored and hidden
}

// PickOptions tunes a Pick.
type PickOptions struct {
	Placeholder string
	Initial     int    // index selected when the list opens
	Filter      string // initial filter text
}

// MultiPickOptions tunes a MultiPick.
type MultiPickOptions struct {
	Placeholder string
	Initial     []int  // indexes selected when the list opens
	Filter      string // initial filter text
}

// Select presents options and returns the chosen index, or ErrCancelled on Esc.
func (u *UI) Select(prompt string, options []Option) (int, error) {
	return u.SelectContext(context.Background(), prompt, options)
}

// SelectContext is Select, abandoned when ctx ends.
func (u *UI) SelectContext(ctx context.Context, prompt string, options []Option) (int, error) {
	if len(options) == 0 {
		return 0, ErrCancelled
	}
	s := &selectState{prompt: prompt, options: options}
	if err := u.run(ctx, s); err != nil {
		return 0, err
	}
	return s.cursor, nil
}

// Confirm asks a yes or no question, defaulting to no on Esc.
func (u *UI) Confirm(prompt string) (bool, error) {
	return u.ConfirmContext(context.Background(), prompt)
}

// ConfirmContext is Confirm, abandoned when ctx ends.
func (u *UI) ConfirmContext(ctx context.Context, prompt string) (bool, error) {
	i, err := u.SelectContext(ctx, prompt, []Option{{Label: "Yes"}, {Label: "No"}})
	return i == 0, err
}

// Input prompts for a line of text, returning ErrCancelled on Esc.
func (u *UI) Input(label, placeholder string) (string, error) {
	return u.InputContext(context.Background(), label, placeholder)
}

// InputContext is Input, abandoned when ctx ends.
func (u *UI) InputContext(ctx context.Context, label, placeholder string) (string, error) {
	s := &inputState{label: label, placeholder: placeholder}
	if err := u.run(ctx, s); err != nil {
		return "", err
	}
	return s.value, nil
}

// Pick presents a list narrowed by typed text, returning the chosen index into
// items, or ErrCancelled on Esc.
func (u *UI) Pick(prompt string, items []PickItem, opts PickOptions) (int, error) {
	return u.PickContext(context.Background(), prompt, items, opts)
}

// PickContext is Pick, abandoned when ctx ends.
func (u *UI) PickContext(ctx context.Context, prompt string, items []PickItem, opts PickOptions) (int, error) {
	if len(items) == 0 {
		return 0, ErrCancelled
	}
	s := &pickState{prompt: prompt, items: items, filter: opts.Filter, placeholder: opts.Placeholder}
	s.refilter()
	if i := slices.Index(s.matches, opts.Initial); i >= 0 {
		s.cursor = i
	}
	if err := u.run(ctx, s); err != nil {
		return 0, err
	}
	return s.chosen, nil
}

// MultiPick presents a filterable multi-select list and returns the chosen
// indexes, or ErrCancelled on Esc. Space/Tab toggle the highlighted row, ↑/↓
// move, Enter confirms and Esc cancels; typed text narrows the filter.
// Rows group under a dim header when PickItem.Group changes.
func (u *UI) MultiPick(prompt string, items []PickItem, opts MultiPickOptions) ([]int, error) {
	return u.MultiPickContext(context.Background(), prompt, items, opts)
}

// MultiPickContext is MultiPick, abandoned when ctx ends.
func (u *UI) MultiPickContext(ctx context.Context, prompt string, items []PickItem, opts MultiPickOptions) ([]int, error) {
	if len(items) == 0 {
		return nil, ErrCancelled
	}
	s := &multiPickState{prompt: prompt, items: items, filter: opts.Filter, placeholder: opts.Placeholder, selected: make(map[int]struct{})}
	for _, i := range opts.Initial {
		s.selected[i] = struct{}{}
	}
	s.refilter()
	if err := u.run(ctx, s); err != nil {
		return nil, err
	}
	return s.chosen, nil
}

// selectState is a fixed list of options with a moving cursor.
type selectState struct {
	prompt  string
	options []Option
	cursor  int
}

func (s *selectState) rows(t Theme, width, maxRows int) ([]string, int, int) {
	rows := []string{t.Accent.Wrap(s.prompt)}
	start, end := windowFor(s.cursor, len(s.options), maxRows-1)
	for i := start; i < end; i++ {
		rows = append(rows, optionRow(t, s.options[i], i == s.cursor, width))
	}
	if end-start < len(s.options) {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(len(s.options)-(end-start))))
	}
	return rows, 0, 0
}

func (s *selectState) key(k key) (bool, error) {
	switch k.typ {
	case keyUp:
		s.cursor = wrapIndex(s.cursor-1, len(s.options))
	case keyDown:
		s.cursor = wrapIndex(s.cursor+1, len(s.options))
	case keyEnter:
		return true, nil
	case keyEscape, keyInterrupt:
		return true, ErrCancelled
	case keyRune:
		if n, err := strconv.Atoi(k.text); err == nil && n >= 1 && n <= min(len(s.options), interactionCap) {
			s.cursor = n - 1
			return true, nil
		}
	}
	return false, nil
}

func (s *selectState) summary(t Theme) string {
	return t.Dim.Wrap(noticeMarker + " " + s.prompt + " " + s.options[s.cursor].Label)
}

// inputState is a single line text prompt.
type inputState struct {
	label       string
	placeholder string
	value       string
}

func (s *inputState) rows(t Theme, width, _ int) ([]string, int, int) {
	shown := s.value
	if shown == "" && s.placeholder != "" {
		shown = t.Dim.Wrap(s.placeholder)
	}
	line := t.Accent.Wrap(s.label+" ") + shown
	return []string{truncateDisplay(line, width)}, 0, displayWidth(t.Accent.Wrap(s.label+" ")) + displayWidth(s.value)
}

func (s *inputState) key(k key) (bool, error) {
	switch k.typ {
	case keyRune, keyPaste:
		s.value += strings.ReplaceAll(k.text, "\n", " ")
	case keyBackspace:
		if s.value != "" {
			_, size := lastRune(s.value)
			s.value = s.value[:len(s.value)-size]
		}
	case keyKillLine:
		s.value = ""
	case keyEnter:
		return true, nil
	case keyEscape, keyInterrupt:
		return true, ErrCancelled
	}
	return false, nil
}

func (s *inputState) summary(t Theme) string {
	return t.Dim.Wrap(noticeMarker + " " + s.label + " " + s.value)
}

// pickState is a list narrowed by a live filter.
type pickState struct {
	prompt      string
	placeholder string
	items       []PickItem
	filter      string
	matches     []int // indexes into items, best first
	cursor      int   // index into matches
	chosen      int   // index into items
}

// refilter recomputes the match set, keeping the order stable for equal scores.
func (s *pickState) refilter() {
	s.matches = refilterMatches(s.items, s.filter)
	s.cursor = 0
}

// refilterMatches returns item indexes matching filter, best first. The order is
// kept stable for equal scores so a list does not jump around.
func refilterMatches(items []PickItem, filter string) (matches []int) {
	type scored struct {
		index int
		score int
	}
	var hits []scored
	for i, it := range items {
		if score, ok := bestScore(it, filter); ok {
			hits = append(hits, scored{index: i, score: score})
		}
	}
	slices.SortStableFunc(hits, func(a, b scored) int { return b.score - a.score })

	matches = make([]int, len(hits))
	for i, h := range hits {
		matches[i] = h.index
	}
	return matches
}

// bestScore returns the best score across an item's matchable strings.
func bestScore(it PickItem, filter string) (int, bool) {
	best, found := 0, false
	for _, field := range append([]string{it.Label, it.Detail}, it.Terms...) {
		if score, ok := matchScore(field, filter); ok && (!found || score > best) {
			best, found = score, true
		}
	}
	return best, found
}

func (s *pickState) rows(t Theme, width, maxRows int) ([]string, int, int) {
	header := t.Accent.Wrap(s.prompt) + t.Dim.Wrap("  "+strconv.Itoa(len(s.matches))+
		" of "+strconv.Itoa(len(s.items)))
	shown := s.filter
	if shown == "" && s.placeholder != "" {
		shown = t.Dim.Wrap(s.placeholder)
	}
	filterRow := t.User.Wrap(userMarker) + shown
	rows := []string{header, filterRow}

	listRows := maxRows - len(rows)
	start, end := windowFor(s.cursor, len(s.matches), listRows)
	for i := start; i < end; i++ {
		it := s.items[s.matches[i]]
		rows = append(rows, pickItemRow(t, it, i == s.cursor, width))
	}
	if len(s.matches) == 0 {
		rows = append(rows, t.Dim.Wrap(selectIndent+"no matches"))
	} else if end-start < len(s.matches) {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(len(s.matches)-(end-start))))
	}
	return rows, 1, displayWidth(t.User.Wrap(userMarker)) + displayWidth(s.filter)
}

func (s *pickState) key(k key) (bool, error) {
	switch k.typ {
	case keyUp:
		s.cursor = wrapIndex(s.cursor-1, len(s.matches))
	case keyDown:
		s.cursor = wrapIndex(s.cursor+1, len(s.matches))
	case keyPageUp:
		s.cursor = max(0, s.cursor-pickPage)
	case keyPageDown:
		s.cursor = min(max(len(s.matches)-1, 0), s.cursor+pickPage)
	case keyRune, keyPaste:
		s.filter += strings.ReplaceAll(k.text, "\n", "")
		s.refilter()
	case keyBackspace:
		if s.filter != "" {
			_, size := lastRune(s.filter)
			s.filter = s.filter[:len(s.filter)-size]
			s.refilter()
		}
	case keyKillLine:
		s.filter = ""
		s.refilter()
	case keyEnter:
		if len(s.matches) == 0 {
			return false, nil
		}
		s.chosen = s.matches[s.cursor]
		return true, nil
	case keyEscape, keyInterrupt:
		return true, ErrCancelled
	}
	return false, nil
}

func (s *pickState) summary(t Theme) string {
	if len(s.items) == 0 {
		return ""
	}
	return t.Dim.Wrap(noticeMarker + " " + s.prompt + " " + s.items[s.chosen].Label)
}

const pickPage = 5

// optionRow renders one list row, marked when it is the cursor.
func optionRow(t Theme, o Option, selected bool, width int) string {
	marker, style := selectIndent, t.Dim
	if selected {
		marker, style = selectMarker, t.Accent
	}
	line := style.Wrap(marker + o.Label)
	if o.Detail != "" {
		line += t.Dim.Wrap("  " + o.Detail)
	}
	return truncateDisplay(line, width)
}

// pickItemRow renders one Pick row: a cursor marker, an optional role tag colored
// independently of selection so user/assistant reads at a glance, then the label
// and detail under the base (dim/accent) style.
func pickItemRow(t Theme, it PickItem, selected bool, width int) string {
	marker, base := selectIndent, t.Dim
	if selected {
		marker, base = selectMarker, t.Accent
	}
	tagStyle := markStyle(t, it.Mark)
	line := marker + tagStyle.Wrap(it.Tag) + base.Wrap(it.Label)
	if it.Detail != "" {
		line += t.Dim.Wrap("  " + it.Detail)
	}
	return truncateDisplay(line, width)
}

// markStyle returns the palette style for a role tag; MarkNone is a no-op.
func markStyle(t Theme, m ItemMark) Style {
	switch m {
	case MarkUser:
		return t.UserTag
	case MarkAssistant:
		return t.Assist
	default:
		return Style{}
	}
}

// windowFor returns the visible slice bounds keeping cursor in view.
func windowFor(cursor, total, rows int) (start, end int) {
	if rows <= 0 || total == 0 {
		return 0, 0
	} else if total <= rows {
		return 0, total
	}
	start = max(0, cursor-rows/2)
	if start+rows > total {
		start = total - rows
	}
	return start, start + rows
}

// wrapIndex moves an index cyclically, so the ends of a list join up.
func wrapIndex(i, n int) int {
	if n == 0 {
		return 0
	}
	return ((i % n) + n) % n
}

func moreLabel(n int) string { return "... " + strconv.Itoa(n) + " more" }

// lastRune returns the final rune of s and its byte width.
func lastRune(s string) (rune, int) {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0, 0
	}
	last := runes[len(runes)-1]
	return last, len(string(last))
}

// pickRow is one navigable MultiPick row: a real item (item >= 0) or a synthetic
// group header (Group set, item < 0). Headers are first-class so a whole group can
// be toggled in one keypress.
type pickRow struct {
	group string // non-empty only for a header row
	item  int    // index into items; negative marks a header
}

// multiPickState is a multi-select list narrowed by a live filter. Space/Tab
// toggle the highlighted row, Enter confirms, Esc cancels; typed text (other
// than space) narrows the filter. Rows group under a navigable checkbox header
// when PickItem.Group changes between matches.
type multiPickState struct {
	prompt      string
	placeholder string
	items       []PickItem
	filter      string
	picks       []pickRow // visible rows, headers included, best first
	cursor      int       // index into picks
	selected    map[int]struct{}
	chosen      []int // indexes into items, in item order
}

// refilter recomputes the row set, keeping the order stable for equal scores.
func (s *multiPickState) refilter() {
	s.picks = buildRows(s.items, s.filter)
	s.cursor = 0
}

// buildRows orders matching items best-first with a synthetic header before each
// group change so the list reads as grouped sections.
func buildRows(items []PickItem, filter string) []pickRow {
	var out []pickRow
	prev := ""
	for _, idx := range refilterMatches(items, filter) {
		g := items[idx].Group
		if g != "" && g != prev {
			out = append(out, pickRow{group: g, item: -1})
			prev = g
		}
		out = append(out, pickRow{item: idx})
	}
	return out
}

func (s *multiPickState) rows(t Theme, width, maxRows int) ([]string, int, int) {
	header := t.Accent.Wrap(s.prompt) + t.Dim.Wrap("  "+strconv.Itoa(len(s.selected))+
		" selected · "+strconv.Itoa(len(s.picks))+" of "+strconv.Itoa(len(s.items)))
	shown := s.filter
	if shown == "" && s.placeholder != "" {
		shown = t.Dim.Wrap(s.placeholder)
	}
	filterRow := t.User.Wrap(userMarker) + shown
	rows := []string{header, filterRow}

	listRows := maxRows - len(rows)
	start, end := windowFor(s.cursor, len(s.picks), listRows)
	for i := start; i < end; i++ {
		r := s.picks[i]
		if r.item < 0 { // group header row
			members := s.groupMembers(r.group)
			rows = append(rows, multiPickHeaderRow(t, r.group,
				groupTri(members, s.selected), i == s.cursor, width))
		} else {
			it := s.items[r.item]
			rows = append(rows, multiPickRow(t, it, i == s.cursor, s.isSelected(r.item), width))
		}
	}
	if len(s.picks) == 0 {
		rows = append(rows, t.Dim.Wrap(selectIndent+"no matches"))
	} else if end-start < len(s.picks) {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(len(s.picks)-(end-start))))
	}
	return rows, 1, displayWidth(t.User.Wrap(userMarker)) + displayWidth(s.filter)
}

// groupMembers returns the visible item indexes belonging to a header's group.
func (s *multiPickState) groupMembers(group string) []int {
	var out []int
	for _, r := range s.picks {
		if r.item >= 0 && s.items[r.item].Group == group {
			out = append(out, r.item)
		}
	}
	return out
}

// allSelected reports whether every member of a set is selected.
func (s *multiPickState) allSelected(members []int) bool {
	for _, m := range members {
		if !s.isSelected(m) {
			return false
		}
	}
	return len(members) > 0
}

// groupTri reports a header's checkbox state: -1 none, 0 partial, 1 all.
func groupTri(members []int, selected map[int]struct{}) int {
	sel := 0
	for _, m := range members {
		if _, ok := selected[m]; ok {
			sel++
		}
	}
	switch {
	case sel == 0:
		return -1
	case sel == len(members):
		return 1
	default:
		return 0
	}
}

func (s *multiPickState) key(k key) (bool, error) {
	switch k.typ {
	case keyUp:
		s.cursor = wrapIndex(s.cursor-1, len(s.picks))
	case keyDown:
		s.cursor = wrapIndex(s.cursor+1, len(s.picks))
	case keyPageUp:
		s.cursor = max(0, s.cursor-pickPage)
	case keyPageDown:
		s.cursor = min(max(len(s.picks)-1, 0), s.cursor+pickPage)
	case keyRune:
		if k.text == " " {
			s.toggleCurrent() // space selects/deselects; it never narrows the filter
			return false, nil
		}
		s.filter += k.text
		s.refilter()
	case keyPaste:
		s.filter += strings.ReplaceAll(k.text, "\n", "")
		s.refilter()
	case keyBackspace:
		if s.filter != "" {
			_, size := lastRune(s.filter)
			s.filter = s.filter[:len(s.filter)-size]
			s.refilter()
		}
	case keyKillLine:
		s.filter = ""
		s.refilter()
	case keyTab:
		s.toggleCurrent()
	case keyEnter:
		if len(s.picks) == 0 {
			return false, nil
		}
		s.chosen = make([]int, 0, len(s.selected))
		for idx := range s.selected {
			s.chosen = append(s.chosen, idx)
		}
		slices.Sort(s.chosen)
		return true, nil
	case keyEscape, keyInterrupt:
		return true, ErrCancelled
	}
	return false, nil
}

// toggleCurrent flips the highlighted row. A header toggles its whole group: it
// deselects every member when all are already selected, otherwise selects them.
func (s *multiPickState) toggleCurrent() {
	if len(s.picks) == 0 {
		return
	}
	r := s.picks[s.cursor]
	if r.item < 0 { // header row toggles the entire group at once
		members := s.groupMembers(r.group)
		if s.allSelected(members) {
			for _, m := range members {
				delete(s.selected, m)
			}
		} else {
			for _, m := range members {
				s.selected[m] = struct{}{}
			}
		}
		return
	}
	if s.isSelected(r.item) {
		delete(s.selected, r.item)
	} else {
		s.selected[r.item] = struct{}{}
	}
}

func (s *multiPickState) summary(t Theme) string {
	if len(s.selected) == 0 {
		return t.Dim.Wrap(noticeMarker + " " + s.prompt + " (none)")
	}
	return t.Dim.Wrap(noticeMarker + " " + s.prompt + " " + strconv.Itoa(len(s.selected)) + " selected")
}

func (s *multiPickState) isSelected(idx int) bool {
	_, ok := s.selected[idx]
	return ok
}

// multiPickRow renders one multi-select row with a checkbox and cursor marker.
func multiPickRow(t Theme, it PickItem, selected, checked bool, width int) string {
	box := "[ ] "
	if checked {
		box = "[x] "
	}
	marker, style := selectIndent, t.Dim
	if selected {
		marker, style = selectMarker, t.Accent
	}
	line := style.Wrap(marker + box + it.Label)
	if it.Detail != "" {
		line += t.Dim.Wrap("  " + it.Detail)
	}
	return truncateDisplay(line, width)
}

// multiPickHeaderRow renders a group header as a navigable tri-state checkbox:
// [x] all selected, [ ] none, [~] partial.
func multiPickHeaderRow(t Theme, group string, tri int, cursor bool, width int) string {
	box := "[ ] "
	switch tri {
	case 1:
		box = "[x] "
	case 0:
		box = "[~] " // partial selection within the group
	}
	marker, style := selectIndent, t.Dim
	if cursor {
		marker, style = selectMarker, t.Accent
	}
	line := style.Wrap(marker+box) + t.Dim.Wrap(group)
	return truncateDisplay(line, width)
}
