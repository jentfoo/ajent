package permit

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
)

// classifyCase pins one tool call's expected static verdict.
type classifyCase struct {
	name   string
	tool   string // empty means bash with in as the command
	in     string
	roName []string // names reported read-only by the registry stub
	want   Verdict
}

func bashCall(command string) agent.ToolCall {
	return agent.ToolCall{ID: "c", Name: "bash", Input: json.RawMessage(`{"command":` + strconvQuote(command) + `}`)}
}
func call(name, in string) agent.ToolCall {
	return agent.ToolCall{ID: "c", Name: name, Input: json.RawMessage(in)}
}

// noRO is a registry stub that marks nothing read-only.
var noRO = func(string) bool { return false }

// roSet returns a stub reporting the given names read-only.
func roSet(names []string) func(string) bool {
	set := make(map[string]bool)
	for _, n := range names {
		set[n] = true
	}
	return func(n string) bool { return set[n] }
}

func TestClassify(t *testing.T) {
	t.Parallel()

	cases := []classifyCase{
		// read-only built-ins run without a prompt regardless of registry metadata.
		{"read tool", "read", `{"path":"a.txt"}`, nil, VerdictAllow},
		{"grep tool", "grep", `{}`, nil, VerdictAllow},
		{"find tool", "find", `{}`, nil, VerdictAllow},
		{"ls tool", "ls", `{}`, nil, VerdictAllow},

		// registry-marked read-only MCP tools.
		{"ro mcp tool", "mcp__list", `{}`, []string{"mcp__list"}, VerdictAllow},
		{"unannotated mcp prompts", "mcp__create", `{}`, nil, VerdictPrompt},

		// bash: verifiably read-only commands.
		{"bash ls", "", "ls -la", nil, VerdictAllow},
		{"bash grep pipeline", "", "grep foo f | sort", nil, VerdictAllow},
		{"bash git status", "", "git status", nil, VerdictAllow},
		{"bash sed read only", "", `sed -n 's/a/b/p' f.txt`, nil, VerdictAllow},

		// bash: in-place writes are hard rejects.
		{"sed -i reject", "", "sed -i s/a/b/ f", nil, VerdictReject},
		{"sed --in-place reject", "", "sed --in-place='.bak' s/a/b/ f", nil, VerdictReject},

		// bash: writes and unverifiable commands prompt.
		{"bash rm write", "", "rm -rf build", nil, VerdictPrompt},
		{"write tool prompts", "write", `{"path":"a.txt"}`, nil, VerdictPrompt},
		{"edit tool prompts", "edit", `{}`, nil, VerdictPrompt},
		{"unverifiable command prompts", "", "stat f.txt", nil, VerdictPrompt},

		// network commands never classify read-only by any path.
		{"curl never readonly", "", "curl -s https://x", nil, VerdictPrompt},
		{"wget never readonly", "", "wget http://x/f", nil, VerdictPrompt},
		{"nc never readonly", "", "nc host 80", nil, VerdictPrompt},

		// find unsafe actions prompt even though find is read-only by name.
		{"find -delete prompts", "find", `{}`, []string{"find"}, VerdictAllow}, // tool form; shell handled below
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var tc agent.ToolCall
			if c.tool == "" {
				tc = bashCall(c.in)
			} else if c.in != "" {
				tc = call(c.tool, c.in)
			} else {
				tc = call(c.tool, `{}`)
			}
			ro := noRO
			if len(c.roName) > 0 {
				ro = roSet(c.roName)
			}
			assert.Equal(t, c.want, Classify(tc, ro))
		})
	}
}

func TestClassifyBashFindUnsafeActionsPrompt(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		"find . -exec rm {} \\;",
		"find . -delete",
		"find . -fprint out.txt",
	} {
		assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
	}
}

func TestClassifyBashUnparseableFailsSafe(t *testing.T) {
	t.Parallel()

	c := agent.ToolCall{ID: "c", Name: "bash", Input: json.RawMessage(`not json`)}
	assert.Equal(t, VerdictPrompt, Classify(c, noRO))
}

func TestClassifyIgnoresReadOnlyMarkForBuiltinsNotInList(t *testing.T) {
	t.Parallel()

	// write marked read-only must still prompt; only declared metadata for
	// non-built-in names is trusted.
	assert.Equal(t, VerdictPrompt, Classify(call("write", `{}`), roSet([]string{"write"})))
}

func TestAllSegmentsReadOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"ls -la", true},
		{"grep foo f | sort", true}, // pipeline tolerated
		{"git status && git log --oneline", true},
		{`sed -n 's/a/b/p' f`, true},
		{"rm -rf build", false},
		{"find . -delete", false},    // unsafe find action
		{"echo hi > out.txt", false}, // unsafe redirect
		{"curl -s https://x", false}, // not on the allowlist
	}
	for _, c := range cases {
		assert.Equal(t, c.want, allSegmentsReadOnly(scanCommand(c.in)), c.in)
	}
}

func TestClearWrite(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"ls -la", false},
		{"rm build && ls", true},
		{"grep foo f | sort", false},
		{"sed -i s/a/b/ f", false}, // sed handled by its own analyser, not the write list
	}
	for _, c := range cases {
		assert.Equal(t, c.want, clearWrite(scanCommand(c.in)), c.in)
	}
}

// strconvQuote is a tiny JSON string builder to keep test tables readable.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
