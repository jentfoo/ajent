package permit

import (
	"encoding/json"
	"testing"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"

	"github.com/jentfoo/ajent/pkg/agent"
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
	set := bulk.SliceToSet(names)
	return func(n string) bool { _, ok := set[n]; return ok }
}

func TestClassify(t *testing.T) {
	t.Parallel()

	cases := []classifyCase{
		// read-only built-ins run without a prompt regardless of registry metadata
		{"read tool", "read", `{"path":"a.txt"}`, nil, VerdictAllow},
		{"grep tool", "grep", `{}`, nil, VerdictAllow},
		{"find tool", "find", `{}`, nil, VerdictAllow},
		{"ls tool", "ls", `{}`, nil, VerdictAllow},
		// registry-marked read-only MCP tools
		{"ro mcp tool", "mcp__list", `{}`, []string{"mcp__list"}, VerdictAllow},
		{"unannotated mcp prompts", "mcp__create", `{}`, nil, VerdictPrompt},
		// bash: verifiably read-only commands
		{"bash ls", "", "ls -la", nil, VerdictAllow},
		{"bash grep pipeline", "", "grep foo f | sort", nil, VerdictAllow},
		{"bash git status", "", "git status", nil, VerdictAllow},
		{"bash sed read only", "", `sed -n 's/a/b/p' f.txt`, nil, VerdictAllow},
		// bash: exec or write vectors in awk/rg/sort prompt even though the
		// head is otherwise name-trusted.
		{"awk system prompts", "", `awk 'BEGIN{system("x")}'`, nil, VerdictPrompt},
		{"awk print field allows", "", "awk '{print $1}' f", nil, VerdictAllow},
		{"rg pre prompts", "", "rg --pre c p", nil, VerdictPrompt},
		{"sort output prompts", "", "sort -o /tmp/e i", nil, VerdictPrompt},
		{"date set prompts", "", "date -s 2025-01-01", nil, VerdictPrompt},
		{"date set long prompts", "", "date --set=2025-01-01", nil, VerdictPrompt},
		{"date bsd operand prompts", "", "date 031012002025", nil, VerdictPrompt},
		{"date format allows", "", "date +%Y-%m-%d", nil, VerdictAllow},
		{"file compile prompts", "", "file -C -m ./magic", nil, VerdictPrompt},
		{"file plain allows", "", "file -b f.txt", nil, VerdictAllow},
		// bash: in-place writes are hard rejects
		{"sed -i reject", "", "sed -i s/a/b/ f", nil, VerdictReject},
		{"sed --in-place reject", "", "sed --in-place='.bak' s/a/b/ f", nil, VerdictReject},

		// bash: writes and unverifiable commands prompt.
		{"bash rm write", "", "rm -rf build", nil, VerdictPrompt},
		{"write tool prompts", "write", `{"path":"a.txt"}`, nil, VerdictPrompt},
		{"edit tool prompts", "edit", `{}`, nil, VerdictPrompt},
		{"unverifiable command prompts", "", "stat f.txt", nil, VerdictPrompt},
		// network commands never classify read-only by any path
		{"curl never readonly", "", "curl -s https://x", nil, VerdictPrompt},
		{"wget never readonly", "", "wget http://x/f", nil, VerdictPrompt},
		{"nc never readonly", "", "nc host 80", nil, VerdictPrompt},
		// input redirect to a network pseudo-device: allowlisted head or not
		{"input network tcp prompts", "", "cat < /dev/tcp/host/4444", nil, VerdictPrompt},
		{"input network udp prompts", "", "head -c 1M < /dev/udp/host/53", nil, VerdictPrompt},
		{"input network pipeline prompts", "", "head -c 1M < /dev/tcp/h/p | jq .", nil, VerdictPrompt},
		{"input fd prefix network prompts", "", "cat 0</dev/tcp/h/p", nil, VerdictPrompt},
		{"read write network prompts", "", "cat <> /dev/tcp/h/p", nil, VerdictPrompt},
		{"output network prompts", "", "cat > /dev/tcp/h/p", nil, VerdictPrompt},
		// find unsafe actions prompt even though find is read-only by name
		{"find -delete prompts", "find", `{}`, []string{"find"}, VerdictAllow}, // tool form, shell handled below
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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

func TestClassifyEdgeCases(t *testing.T) {
	t.Parallel()

	// find subcommands that mutate or write must prompt
	t.Run("bash_find_unsafe_actions_prompt", func(t *testing.T) {
		for _, cmd := range []string{
			"find . -exec rm {} \\;",
			"find . -delete",
			"find . -fprint out.txt",
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})

	// a planted binary under an allowlisted name must not inherit the real
	// command's verdict: only slash-free heads are name-trusted, everything
	// path-qualified or script-suffixed prompts like anything unverifiable
	t.Run("path_qualified_head_prompts", func(t *testing.T) {
		for _, cmd := range []string{
			`./bin/cat x`,
			`../tooling/jq f`,
			`/tmp/x/git status`,
			`myscript.sh`, // a bare .sh head keeps its literal name, allowlisting nothing
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})

	// path-qualified launcher prefixes launder the head the same way and must
	// not unwrap into a trusted name either
	t.Run("path_qualified_launcher_prompts", func(t *testing.T) {
		for _, cmd := range []string{
			`./nohup git status`,
			`/usr/bin/nohup git log`,
			`./timeout 5 git status`,
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})

	// a bare launcher still unwraps into the wrapped command's own verdict
	t.Run("bare_launcher_still_allows", func(t *testing.T) {
		assert.Equal(t, VerdictAllow, Classify(bashCall("nohup git status"), noRO))
	})

	// and its write form still rejects through the same unwrap
	t.Run("bare_launcher_still_rejects", func(t *testing.T) {
		assert.Equal(t, VerdictReject, Classify(bashCall("nohup sed -i s/a/b/ f"), noRO))
	})

	// awk splices backslash-newline before tokens form, so a redirect or pipe on
	// the next physical line still belongs to the print statement, and awk's own
	// lexing decides comments and strings: the read-only verdict must not survive
	t.Run("awk_continuation_still_prompts", func(t *testing.T) {
		for _, cmd := range []string{
			"awk 'BEGIN{print \\\n> \"/tmp/evil\"}'",
			"printf 'x\\n' | awk '{print $1 \\\n| \"sort\"}'",
			"awk 'BEGIN{print \\\r\n> \"/tmp/evil\"}'",
			// unquoted continuation joins the program word in bash itself
			"awk BEGIN\\{print\\\n\\|\"id\"}",
			// a comment ends at the newline, an apostrophe opens no awk string
			"awk \"# don't\nBEGIN{system(\\\"id\\\")}\" /dev/null",
			"awk '# \" x\nBEGIN{system(\"id\")}'",
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})

	// a segment carrying shell expansion the analyser cannot resolve never
	// verifies read-only: the argv it runs differs from the text analysed
	t.Run("expanded_forms_prompt", func(t *testing.T) {
		for _, cmd := range []string{
			"file {--compile,-m} ./magic",
			"file $'-C' -m ./magic",
			"date --{se,t}=2025-01-01",
			"rg --\\\npre=id f",
			"sort --\\\noutput=/tmp/evil f",
			"date --\\\nset=2030-01-01",
			"date +%s && date -s 2030-01-01",
			"date -S 2025-01-01",
			"file -z f.gz",
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})

	// unparseable input fails closed
	t.Run("bash_unparseable_fails_safe", func(t *testing.T) {
		c := agent.ToolCall{ID: "c", Name: "bash", Input: json.RawMessage(`not json`)}
		assert.Equal(t, VerdictPrompt, Classify(c, noRO))
	})

	// a leading env assignment can hijack what the head actually executes (PATH to
	// another binary, LD_PRELOAD into any dynamically-linked one), so even an
	// otherwise-read-only command must fail closed and prompt.
	t.Run("env_prefix_never_read_only", func(t *testing.T) {
		for _, cmd := range []string{
			`PATH=/tmp/evil git status`,
			`LD_PRELOAD=/tmp/evil.so cat x`,
			`BASH_ENV=/tmp/evil.sh ls`,
			`ENV=/tmp/evil.sh sh -c 'echo hi'`,
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})

	// write marked read-only must still prompt, only declared metadata for non-built-in names trusted
	t.Run("ignores_read_only_mark_for_builtins_not_in_list", func(t *testing.T) {
		assert.Equal(t, VerdictPrompt, Classify(call("write", `{}`), roSet([]string{"write"})))
	})

	// a redirect writes, so the read-only verdict never covers one wherever it
	// lands: verification belongs to the write-scope and configured-safe paths
	t.Run("redirect_never_read_only", func(t *testing.T) {
		for _, cmd := range []string{
			"git status > out.log",
			"git status > /tmp/t.log",
			"ls > f*",
		} {
			assert.Equal(t, VerdictPrompt, Classify(bashCall(cmd), noRO), cmd)
		}
	})
}

// strconvQuote is a tiny JSON string builder to keep test tables readable.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
