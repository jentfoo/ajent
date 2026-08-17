package agent

import "strings"

// progressStep is how many argument bytes must accumulate before a call's
// progress is republished, so a per-token stream does not repaint per token.
const progressStep = 256

// ToolProgress reports a tool call the model is still composing, before it can
// run. Done clears it.
type ToolProgress struct {
	CallID string
	Name   string
	Path   string // target argument once complete, empty until then
	// Bytes is the argument JSON received so far, which is what the stream is
	// actually delivering — not the size of any file the call would produce, which
	// is smaller once the JSON escaping and key names come off.
	Bytes int
	Lines int // newline escapes within the arguments
	Done  bool
}

// callProgress accumulates one in-flight call's streamed arguments.
type callProgress struct {
	name string
	path string
	args strings.Builder
	nl   lineEscapeCounter
	sent int // byte count at the last publish
}

// toolProgress tracks every call being streamed in one response.
type toolProgress struct {
	calls   map[string]*callProgress
	byIndex map[int]string // content block index -> call id, the pairing key
}

// start records a new in-flight call and returns its opening progress.
func (t *toolProgress) start(id string, index int, name string) ToolProgress {
	if t.calls == nil {
		t.calls = make(map[string]*callProgress)
		t.byIndex = make(map[int]string)
	}
	t.calls[id] = &callProgress{name: name}
	t.byIndex[index] = id
	return ToolProgress{CallID: id, Name: name}
}

// resolve names the call an event belongs to. Providers pair a start with its
// deltas and end by block index; the id is only repeated on some of them.
func (t *toolProgress) resolve(id string, index int) string {
	if _, ok := t.calls[id]; ok {
		return id
	}
	return t.byIndex[index]
}

// delta folds a partial-JSON fragment into the call and reports the progress to
// publish, ok being false while the call is still under the republish step.
func (t *toolProgress) delta(id string, index int, fragment string) (ToolProgress, bool) {
	id = t.resolve(id, index)
	c := t.calls[id]
	if c == nil {
		return ToolProgress{}, false
	}
	c.args.WriteString(fragment)
	c.nl.count(fragment)
	if c.path == "" {
		c.path = argTarget(c.args.String())
	}
	if c.args.Len()-c.sent < progressStep {
		return ToolProgress{}, false
	}
	c.sent = c.args.Len()
	return c.progress(id, false), true
}

// end reports the call's final progress and forgets it.
func (t *toolProgress) end(id string, index int) (ToolProgress, bool) {
	id = t.resolve(id, index)
	c := t.calls[id]
	if c == nil {
		return ToolProgress{}, false
	}
	delete(t.calls, id)
	delete(t.byIndex, index)
	return c.progress(id, true), true
}

// clear reports a Done progress for every call still in flight, so an aborted
// stream does not strand a row.
func (t *toolProgress) clear() []ToolProgress {
	out := make([]ToolProgress, 0, len(t.calls))
	for id, c := range t.calls {
		out = append(out, c.progress(id, true))
	}
	t.calls = nil
	return out
}

func (c *callProgress) progress(id string, done bool) ToolProgress {
	return ToolProgress{
		CallID: id,
		Name:   c.name,
		Path:   c.path,
		Bytes:  c.args.Len(),
		Lines:  c.nl.lines,
		Done:   done,
	}
}

// lineEscapeCounter counts \n escapes across the chunks of a streamed JSON
// string, where a newline in an argument is the two-character escape rather than
// a real byte. It carries the escape state so a fragment may end mid-escape.
type lineEscapeCounter struct {
	lines int
	esc   bool // previous byte opened an escape
}

func (c *lineEscapeCounter) count(s string) {
	for i := 0; i < len(s); i++ {
		if c.esc { // the escaped char, whatever it is, is consumed here
			if s[i] == 'n' {
				c.lines++
			}
			c.esc = false
		} else if s[i] == '\\' {
			c.esc = true
		}
	}
}

// targetKeys are the argument names that identify what a call acts on, tried in
// order. Looking them up by name rather than position matters because argument
// order is not guaranteed — a marshalled Go map sorts its keys, so a write's
// long "content" commonly streams ahead of its "path".
var targetKeys = []string{"path", "file_path", "file", "pattern", "command"}

// argTarget names what a call acts on, from a partial JSON object. Empty until a
// complete value has arrived.
func argTarget(partial string) string {
	for _, k := range targetKeys {
		if v, ok := jsonStringField(partial, k); ok {
			return v
		}
	}
	return ""
}

// jsonStringField returns the complete string value of key in a partial JSON
// object, ok being false when the key is absent or its value is still streaming.
func jsonStringField(partial, key string) (string, bool) {
	i := strings.Index(partial, `"`+key+`"`)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimLeft(partial[i+len(key)+2:], " \t\r\n")
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	if !strings.HasPrefix(rest, `"`) {
		return "", false // absent, or not a string argument
	}
	rest = rest[1:]
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++ // skip the escaped char
		case '"':
			return rest[:j], true
		}
	}
	return "", false // still streaming
}
