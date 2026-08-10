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

// PickItem is one row of a filterable list.
type PickItem struct {
	Label  string   // primary text
	Detail string   // dim trailing text
	Terms  []string // extra strings the filter matches, not displayed
}

// PickOptions tunes a Pick.
type PickOptions struct {
	Placeholder string
	Initial     int    // index selected when the list opens
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
	type scored struct {
		index int
		score int
	}
	var hits []scored
	for i, it := range s.items {
		if score, ok := bestScore(it, s.filter); ok {
			hits = append(hits, scored{index: i, score: score})
		}
	}
	slices.SortStableFunc(hits, func(a, b scored) int { return b.score - a.score })

	s.matches = make([]int, len(hits))
	for i, h := range hits {
		s.matches[i] = h.index
	}
	s.cursor = 0
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
		rows = append(rows, optionRow(t, Option{Label: it.Label, Detail: it.Detail}, i == s.cursor, width))
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
