package permit

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
)

func TestBuildOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in       string
		wantLen  int    // exactly four options either way; only the session option differs
		expect   string // expected session-option text, "" if broad/plain (asserted via compound flag)
		compound bool   // expect the broad compound grant (no per-name memory)
	}{
		{"ls -la", 4, "Allow `ls` for session", false},
		{"/usr/bin/ifconfig eth0", 4, "Allow `ifconfig` for session", false}, // path stripped
		{"git status", 4, "Allow `git` for session", false},
		// a compound with one non-readonly head names it; read-only segments don't count
		{"ifconfig | head -n 10", 4, "Allow `ifconfig` for session", false},
		{"rm build && ls", 4, "Allow `rm` for session", false}, // ls is read-only, so only rm governs
		// a repeated head collapses into one grant (git add && git commit)
		{"git add x && git commit -m y", 4, "Allow `git` for session", false},
		// two distinct non-readonly heads name both in the option
		{"rm build && mkdir dir", 4, "Allow `rm` and `mkdir` for session", false},
		// three or more commands defeat per-name memory -> broad grant
		{"rm a && mkdir b && touch c", 4, "", true},
		// redirect/substitution is not a simple command -> broad grant
		{"echo hi > out.txt", 4, "", true},
	}
	for _, c := range cases {
		opts := buildOptions(c.in)
		assert.Len(t, opts, c.wantLen, c.in)
		actions := optionActions(c.in) // labels and actions stay aligned
		_ = actions
		if c.compound {
			assert.Contains(t, opts[optAllowSession], "compound")
		} else if c.expect != "" {
			assert.Equal(t, c.expect, opts[optAllowSession])
		}
	}
}

func TestAllowSessionKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bash", `git status`, "bash:git"},
		{"bash", `git -C repo log --oneline`, "bash:git"}, // flags after head still key on git
		{"bash", `/usr/bin/git status`, "bash:git"},       // path stripped
		// a leading env assignment is never unwrapped, so the key can't collide with a
		// real grant: PATH/LD_PRELOAD can hijack what `cmd` executes, so such a line
		// must re-prompt rather than match an existing bash:<name>.
		{"bash", `PATH=/tmp/evil git status`, "bash:"},
		{"write", `{}`, "write"}, // tool name for non-bash
	}
	for _, c := range cases {
		var tc agent.ToolCall
		if c.name == "bash" {
			tc = bashCall(c.in)
		} else {
			tc = call(c.name, c.in)
		}
		assert.Equal(t, c.want, allowSessionKey(tc), c.in)
	}
}

func TestElideSubjectBoundsLinesAndChars(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("line\n", decisionContextRows+5)
	out := elideSubject(long)
	if got := strings.Count(out, "\n") + 1; got > decisionContextRows {
		assert.Failf(t, "too many lines kept", "kept %d want <= %d", got, decisionContextRows)
	}

	// a long first line is kept whole for the dialog to wrap
	wide := strings.Repeat("x", decisionContextChars+50)
	assert.Equal(t, wide, elideSubject(wide))
	// past the first line the character budget still applies
	assert.Equal(t, "head", elideSubject("head\n"+wide+"\ntail"))
}

func TestElideSubjectEmptyAndSingleLine(t *testing.T) {
	t.Parallel()

	assert.Empty(t, elideSubject(""))
	assert.Equal(t, "one line", elideSubject("one line"))
}

// TestClassifierSystemVerbatim pins the shell classifier prompt so a wording change
// is deliberate and reviewed.
func TestClassifierSystemVerbatim(t *testing.T) {
	t.Parallel()
	assert.Contains(t, ClassifierSystem, "You classify a single shell command")
	assert.Contains(t, ClassifierSystem, `"readonly" — only reads or inspects data with no side effects`)
	assert.Contains(t, ClassifierSystem, `downloads, installs or runs software, redirects output`)
	assert.NotContains(t, ClassifierSystem, "with lasting effects")
	assert.Contains(t, ClassifierSystem, `Reserve "unsure" for unrecognized commands. Respond with ONLY the classification word.`)
}

// TestMCPClassifierSystemVerbatim pins the MCP classifier prompt so a wording change
// is deliberate and reviewed.
func TestMCPClassifierSystemVerbatim(t *testing.T) {
	t.Parallel()
	p := MCPClassifierSystem("mcp_tool", "  does a thing  ", `{"type":"object"}`)

	assert.Contains(t, p, "You classify a single tool invocation")
	assert.Contains(t, p, "A readonly verdict requires NO observable change to anything.")
	assert.Contains(t, p, "sends commands with lasting effects")

	// name/description/params embedded (description trimmed)
	assert.Contains(t, p, "Name: mcp_tool")
	assert.Contains(t, p, "Description: does a thing")
	assert.Contains(t, p, `Parameters (JSON Schema):
{"type":"object"}`)
}
