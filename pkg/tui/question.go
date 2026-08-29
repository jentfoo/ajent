package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// The chat row sits below every set of options, so a user who likes none of them
// can reply in their own words.
const (
	chatOptionLabel   = "Chat about this"
	chatOptionDetail  = "ask or suggest something"
	chatPlaceholder   = "message…"
	answerPlaceholder = "answer…"
)

// Question is an agent initiated prompt: free text when Options is empty,
// otherwise a choice among them plus a chat row that takes a typed reply.
type Question struct {
	Text    string // may be several lines
	Options []Option
}

// Answer is a question's outcome; Declined is set when the user pressed Esc,
// Chat when they typed the reply in Text instead of choosing an option.
type Answer struct {
	Text     string
	Index    int
	Declined bool
	Chat     bool
}

// Ask puts a question to the user, queueing behind any interaction already
// waiting. A declined answer is a normal result, not an error; ErrNoUI means no
// terminal can reach the user, so the caller must decide the policy itself.
func (u *UI) Ask(ctx context.Context, q Question) (Answer, error) {
	u.mu.Lock()
	closed := u.closed
	u.mu.Unlock()
	if closed { // never block when there is nobody to ask
		return Answer{}, ErrNoUI
	}
	st := &questionState{text: q.Text, options: slices.Clone(q.Options), chatIndex: -1}
	if len(st.options) > 0 && u.mode != ModePlain {
		// plain mode has no row to type into; there any non-numeric line is the reply
		st.chatIndex = len(st.options)
		st.options = append(st.options, Option{Label: chatOptionLabel, Detail: chatOptionDetail})
	}
	if err := u.run(ctx, st); err != nil {
		return Answer{}, err
	}
	switch {
	case st.declined:
		return Answer{Declined: true}, nil
	case st.chatting:
		return Answer{Text: st.value, Chat: true}, nil
	case len(st.options) == 0:
		return Answer{Text: st.value}, nil
	}
	return Answer{Index: st.cursor}, nil
}

// questionState is an agent initiated prompt, either a free text line or a fixed
// set of options.
type questionState struct {
	text      string // may be several lines
	options   []Option
	chatIndex int  // position of the chat row in options, -1 when there is none
	chatting  bool // typing a reply instead of choosing an option
	cursor    int  // option cursor when options are offered
	value     string
	declined  bool // Esc: declined to answer, a normal result
}

func (s *questionState) rows(t Theme, width, maxRows int) ([]string, int, int) {
	// the prompt rides above the answer row, capped so the live block stays bounded
	out := s.promptRows(t, width, max(1, maxRows-1))

	if len(s.options) > 0 && !s.chatting { // a choice among offered options
		budget := max(1, maxRows-len(out))
		list, hidden := s.optionBlock(t, width, budget)
		if hidden > 0 && budget > 1 { // re-render a row shorter so the marker fits
			list, hidden = s.optionBlock(t, width, budget-1)
		}
		out = append(out, list...)
		if hidden > 0 {
			out = append(out, t.Dim.Wrap(selectIndent+moreLabel(hidden)))
		}
		return out, 0, 0
	}
	// free-text answer, reusing the input row's editing view
	marker := t.User.Wrap(userMarker)
	if s.value == "" {
		placeholder := answerPlaceholder
		if s.chatting {
			placeholder = chatPlaceholder
		}
		out = append(out, truncateDisplay(marker+t.Dim.Wrap(placeholder), width))
		return out, len(out) - 1, displayWidth(marker)
	}
	typed := wrapLine(s.value, max(1, width-displayWidth(marker)))
	for i := range typed { // continuations align under the reply, as the input row does
		if i == 0 {
			typed[i] = marker + typed[i]
		} else {
			typed[i] = userContinue + typed[i]
		}
	}
	if avail := max(1, maxRows-len(out)); len(typed) > avail {
		typed = typed[len(typed)-avail:] // keep the tail, where the caret sits
	}
	out = append(out, typed...)
	return out, len(out) - 1, displayWidth(typed[len(typed)-1])
}

// promptRows renders the question wrapped to width, filling at most budget rows.
// An overflowing prompt ends in a marker naming the lines left out.
func (s *questionState) promptRows(t Theme, width, budget int) []string {
	lines := s.textLines()
	if len(lines) == 0 || budget <= 0 {
		return nil
	}
	var out []string
	for i, ln := range lines {
		for _, row := range wrapLine(t.Accent.Wrap(ln), width) {
			if len(out) >= budget { // line i is only partly shown, so it counts as hidden
				return append(out[:budget-1], t.Dim.Wrap("… +"+strconv.Itoa(len(lines)-i)+" lines"))
			}
			out = append(out, row)
		}
	}
	return out
}

// optionBlock renders the options wrapped to width, filling at most budget rows
// around the cursor, which is always shown. It returns the rows and how many
// options did not fit.
func (s *questionState) optionBlock(t Theme, width, budget int) ([]string, int) {
	if budget <= 0 {
		return nil, len(s.options)
	}
	blocks := make([][]string, len(s.options))
	for i := range s.options {
		blocks[i] = optionRows(t, s.options[i], i == s.cursor, width)
	}
	// grow a window out from the cursor while whole options still fit
	start, end := s.cursor, s.cursor+1
	used := len(blocks[s.cursor])
	for {
		var grew bool
		if end < len(s.options) && used+len(blocks[end]) <= budget {
			used += len(blocks[end])
			end++
			grew = true
		}
		if start > 0 && used+len(blocks[start-1]) <= budget {
			start--
			used += len(blocks[start])
			grew = true
		}
		if !grew {
			break
		}
	}
	var out []string
	for i := start; i < end; i++ {
		out = append(out, blocks[i]...)
	}
	if len(out) > budget { // the cursor's own option outgrew the budget
		out = out[:budget]
	}
	return out, len(s.options) - (end - start)
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
	case keyEscape:
		if s.chatting { // step back to the options rather than abandon the question
			s.chatting, s.value = false, ""
			return false, nil
		}
		s.declined = true // a declined answer is normal, never an aborting error
		return true, nil
	case keyInterrupt:
		s.declined = true
		return true, nil
	}
	if len(s.options) > 0 && !s.chatting { // the options half mirrors selectState
		switch k.typ {
		case keyUp:
			s.cursor = wrapIndex(s.cursor-1, len(s.options))
		case keyDown:
			s.cursor = wrapIndex(s.cursor+1, len(s.options))
		case keyEnter:
			return s.choose(s.cursor), nil
		case keyRune:
			if n, err := strconv.Atoi(k.text); err == nil && n >= 1 && n <= min(len(s.options), interactionCap) {
				return s.choose(n - 1), nil
			}
		}
		return false, nil
	}
	switch k.typ { // the free-text half mirrors inputState's editing keys
	case keyRune, keyPaste:
		s.value += strings.ReplaceAll(k.text, "\n", " ")
	case keyBackspace:
		s.value = trimLastCluster(s.value)
	case keyKillLine:
		s.value = ""
	case keyEnter:
		return true, nil
	}
	return false, nil
}

// choose settles on option i, or opens the typed reply when i is the chat row.
// It reports whether the question is answered.
func (s *questionState) choose(i int) bool {
	s.cursor = i
	if i == s.chatIndex {
		s.chatting = true
		return false
	}
	return true
}

func (s *questionState) summary(t Theme) string {
	if s.declined {
		// a declined ask is worth recording; an answered one echoes nothing because
		// the caller logs its own outcome.
		return t.Dim.Wrap(noticeMarker + " question declined")
	}
	return ""
}

func splitLines(s string) []string {
	if !strings.Contains(s, "\n") {
		return []string{s}
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
