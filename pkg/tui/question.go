package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// Question is an agent initiated prompt: free text when Options is empty,
// otherwise a choice among them.
type Question struct {
	Text    string // may be several lines
	Options []Option
}

// Answer is a question's outcome; Declined is set when the user pressed Esc.
type Answer struct {
	Text     string
	Index    int
	Declined bool
}

// Ask puts a question to the user, queueing behind any interaction already
// waiting. A declined answer is a normal result, not an error; ErrNoUI means no
// terminal can reach the user, so the caller must decide the policy itself.
func (u *UI) Ask(ctx context.Context, q Question) (Answer, error) {
	if u.closed { // never block when there is nobody to ask
		return Answer{}, ErrNoUI
	}
	st := &questionState{text: q.Text, options: slices.Clone(q.Options)}
	if err := u.run(ctx, st); err != nil {
		return Answer{}, err
	}
	if st.declined {
		return Answer{Declined: true}, nil
	}
	if len(st.options) == 0 {
		return Answer{Text: st.value}, nil
	}
	return Answer{Index: st.cursor}, nil
}

// questionState is an agent initiated prompt, either a free text line or a fixed
// set of options.
type questionState struct {
	text     string // may be several lines
	options  []Option
	cursor   int    // option cursor when options are offered
	value    string // typed answer in the free-text form
	declined bool   // Esc: declined to answer, a normal result
}

func (s *questionState) rows(t Theme, width, maxRows int) ([]string, int, int) {
	var out []string
	lines := s.textLines()
	if len(lines) > 0 { // the prompt rides above the answer row, capped so the live block stays bounded
		textBudget := max(1, min(len(lines), maxRows-1))
		for _, ln := range lines {
			if len(out) >= textBudget {
				break
			}
			out = append(out, truncateDisplay(t.Accent.Wrap(ln), width))
		}
		if cut := len(lines) - len(out); cut > 0 { // mark the hidden remainder rather than silently drop it
			out[len(out)-1] = t.Dim.Wrap("… +" + strconv.Itoa(cut) + " lines")
		}
	}
	if len(s.options) > 0 { // a choice among offered options
		listRows := maxRows - len(out)
		start, end := windowFor(s.cursor, len(s.options), listRows)
		for i := start; i < end; i++ {
			out = append(out, optionRow(t, s.options[i], i == s.cursor, width))
		}
		if end-start < len(s.options) {
			out = append(out, t.Dim.Wrap(selectIndent+moreLabel(len(s.options)-(end-start))))
		}
		return out, 0, 0
	}
	// free-text answer, reusing the input row's editing view
	shown := s.value
	if shown == "" {
		shown = t.Dim.Wrap("answer…")
	}
	out = append(out, truncateDisplay(t.User.Wrap(userMarker)+shown, width))
	return out, len(out) - 1, displayWidth(t.User.Wrap(userMarker)) + displayWidth(s.value)
}

// textLines splits the prompt into rows; blank lines are dropped so a long
// question does not burn live-block height on empty separators.
func (s *questionState) textLines() []string {
	lines := splitLines(s.text)
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	return out
}

func (s *questionState) key(k key) (bool, error) {
	switch k.typ {
	case keyEscape, keyInterrupt:
		s.declined = true // a declined answer is normal, never an aborting error
		return true, nil
	}
	if len(s.options) > 0 { // the options half mirrors selectState
		switch k.typ {
		case keyUp:
			s.cursor = wrapIndex(s.cursor-1, len(s.options))
		case keyDown:
			s.cursor = wrapIndex(s.cursor+1, len(s.options))
		case keyEnter:
			return true, nil
		case keyRune:
			if n, err := strconv.Atoi(k.text); err == nil && n >= 1 && n <= min(len(s.options), interactionCap) {
				s.cursor = n - 1
				return true, nil
			}
		}
		return false, nil
	}
	switch k.typ { // the free-text half mirrors inputState's editing keys
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
	}
	return false, nil
}

func (s *questionState) summary(t Theme) string {
	switch {
	case s.declined:
		return t.Dim.Wrap(noticeMarker + " question declined")
	case len(s.options) > 0 && s.cursor < len(s.options):
		return t.Dim.Wrap(noticeMarker + " answer: " + s.options[s.cursor].Label)
	default:
		return t.Dim.Wrap(noticeMarker + " answer: " + s.value)
	}
}

func splitLines(s string) []string {
	if !strings.Contains(s, "\n") {
		return []string{s}
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
