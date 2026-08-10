package llm

import "strings"

// thinkSplitter routes streamed content to text or thinking for models that
// mark reasoning with inline tags. Bytes that could still be the start of a tag
// are withheld until the next delta resolves them, so a tag split across
// several deltas never leaks into the visible text.
type thinkSplitter struct {
	open   string
	close  string
	inside bool
	held   string
}

// newThinkSplitter returns a splitter for the given tag pair, falling back to
// the conventional think tags.
func newThinkSplitter(open, close string) *thinkSplitter {
	if open == "" || close == "" {
		open, close = thinkOpenTag, thinkCloseTag
	}
	return &thinkSplitter{open: open, close: close}
}

// Write splits one content delta into its visible and reasoning parts.
func (s *thinkSplitter) Write(delta string) (text, thinking string) {
	if delta == "" {
		return "", ""
	}
	var tb, kb strings.Builder
	buf := s.held + delta
	s.held = ""

	for buf != "" {
		tag := s.open
		if s.inside {
			tag = s.close
		}
		i := strings.Index(buf, tag)
		if i < 0 {
			// withhold the longest suffix that could still become the tag
			keep := partialTagLen(buf, tag)
			s.emit(&tb, &kb, buf[:len(buf)-keep])
			s.held = buf[len(buf)-keep:]
			break
		}
		s.emit(&tb, &kb, buf[:i])
		s.inside = !s.inside
		buf = buf[i+len(tag):]
	}
	return tb.String(), kb.String()
}

// Flush releases whatever was withheld at end of stream.
func (s *thinkSplitter) Flush() (text, thinking string) {
	if s.held == "" {
		return "", ""
	}
	held := s.held
	s.held = ""
	if s.inside {
		return "", held
	}
	return held, ""
}

// emit appends to whichever side is currently open.
func (s *thinkSplitter) emit(text, thinking *strings.Builder, chunk string) {
	if chunk == "" {
		return
	} else if s.inside {
		thinking.WriteString(chunk)
	} else {
		text.WriteString(chunk)
	}
}

// partialTagLen returns the length of the longest suffix of buf that is a
// proper prefix of tag.
func partialTagLen(buf, tag string) int {
	n := min(len(buf), len(tag)-1)
	for ; n > 0; n-- {
		if strings.HasPrefix(tag, buf[len(buf)-n:]) {
			return n
		}
	}
	return 0
}
