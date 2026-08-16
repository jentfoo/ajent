package srv

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// scratchPrefix names the per-run directory under /tmp that step 0 creates; its
// unix-nano suffix is both the run's identity and its start timestamp.
const scratchPrefix = "/tmp/ajent-demo-"

// scriptStep is one ordered assistant turn in the demo: optional thinking prose,
// content shown alongside the tool call(s), and the calls themselves. A step with
// no calls ends the turn (finish_reason "stop").
type scriptStep struct {
	think   string // reasoning delta, empty for none
	content string // prose emitted before the calls; final summary when calls are nil
	calls   []stepCall
}

// stepCall is one tool invocation with a preferred native name and a bash
// equivalent (using <dir> as the scratch path), so agents that do not advertise
// the native tool can still complete it through a shell.
type stepCall struct {
	name string              // preferred native tool name
	bash string              // cross-agent fallback command; "" when none makes sense
	args func(r *run) []byte // native JSON arguments; nil uses bash's command
}

// run carries the stateless per-run identity recovered from prior messages: the
// scratch directory and its unix-nano start time.
type run struct {
	dir   string
	start time.Time
}

// newRun starts a fresh run at now, naming its scratch dir with a unique suffix.
func newRun(now time.Time) *run {
	return &run{dir: fmt.Sprintf("%s%d", scratchPrefix, now.UnixNano()), start: now}
}

// script is the ordered list of turns. The step index equals the count of prior
// assistant messages in the request; prose rides with the next tool call so a turn
// never ends before its calls.
func script() []scriptStep {
	nativeRead := func(file string) stepCall { // read a scratch file natively or via cat
		return stepCall{name: "read", bash: "cat <dir>/" + file,
			args: func(r *run) []byte { return jsonArgs(map[string]any{"path": r.dir + "/" + file}) }}
	}
	nativeGrep := func(file, pat string) stepCall {
		return stepCall{name: "grep", bash: fmt.Sprintf("grep -n '%s' <dir>/%s", pat, file),
			args: func(r *run) []byte {
				return jsonArgs(map[string]any{"pattern": pat, "path": r.dir, "glob": file})
			}}
	}
	nativeWrite := func(file string, body string) stepCall { // write a scratch file
		bash := fmt.Sprintf("cat > <dir>/%s <<'GOEOF'\n%s\nGOEOF", file, body)
		return stepCall{name: "write", bash: bash,
			args: func(r *run) []byte {
				return jsonArgs(map[string]any{"path": r.dir + "/" + file, "content": body})
			}}
	}
	nativeBash := func(cmd string) stepCall { // run a shell line against the scratch dir
		return stepCall{name: "bash", bash: cmd,
			args: func(r *run) []byte {
				return jsonArgs(map[string]any{"command": strings.ReplaceAll(cmd, "<dir>", r.dir)})
			}}
	}

	// each non-final turn is deliberately small: a short thought (or none) plus one
	// or two tool calls. Reads dominate so the demo exercises read-only auto-allow
	// and streams real file contents across many turns.
	return []scriptStep{
		{ // 0: brief plan, then first approval dialog (mkdir is clearWrite)
			think: demoThinking0,
			calls: []stepCall{nativeBash("mkdir -p <dir>")},
		},
		{ // 1: write notes.go; dialog with a Preview diff subject
			calls: []stepCall{nativeWrite("notes.go", notesGo)},
		},
		{ // 2: edit with a non-existent oldText; DryRun fails so no prompt
			calls: []stepCall{{name: "edit",
				args: func(r *run) []byte {
					return jsonArgs(map[string]any{"path": r.dir + "/notes.go", "edits": []map[string]any{
						{"oldText": missingOldText, "newText": retryAfter}}})
				}}},
		},
		{ // 3: thinking about the failure, then read (read-only auto-allow)
			think: demoThinking2,
			calls: []stepCall{nativeRead("notes.go")},
		},
		{ // 4: edit with the correct oldText; dialog then a real Diff in history
			calls: []stepCall{{name: "edit",
				args: func(r *run) []byte {
					return jsonArgs(map[string]any{"path": r.dir + "/notes.go", "edits": []map[string]any{
						{"oldText": retryBefore, "newText": retryAfter}}})
				}}},
		},
		{ // 5: longer planning thought, then find what is in the scratch dir
			think: demoThinking,
			calls: []stepCall{{name: "find", bash: "find <dir> -name '*.go'",
				args: func(r *run) []byte { return jsonArgs(map[string]any{"pattern": "*.go", "path": r.dir}) }}},
		},
		{ // 6: write a test file; another Preview dialog
			calls: []stepCall{nativeWrite("retry_test.go", retryTestGo)},
		},
		{ // 7: grep the new test for its helper (read-only, no prompt)
			calls: []stepCall{nativeGrep("retry_test.go", "WithMaxAttempts")},
		},
		{ // 8: read the test back
			calls: []stepCall{nativeRead("retry_test.go")},
		},
		{ // 9: compound read-only shell over both Go files (analyser allows it)
			calls: []stepCall{nativeBash("wc -l <dir>/notes.go && head -5 <dir>/retry_test.go")},
		},
		{ // 10: write the doc file
			calls: []stepCall{nativeWrite("README.md", readmeMD)},
		},
		{ // 11: read it back
			calls: []stepCall{nativeRead("README.md")},
		},
		{ // 12: list the scratch dir (read-only)
			calls: []stepCall{nativeBash("ls -la <dir>")},
		},
		{ // 13: grep and read notes.go in one message (parallel dispatch)
			calls: []stepCall{
				nativeGrep("notes.go", "maxDelay"),
				nativeRead("notes.go"),
			},
		},
		{ // 14: stream the smaller test file into scrollback
			calls: []stepCall{nativeBash("cat <dir>/retry_test.go")},
		},
		{ // 15: re-read notes.go to confirm the edit landed
			calls: []stepCall{nativeRead("notes.go")},
		},
		{ // 16: cat streams the whole file into scrollback, read-only so no prompt
			calls: []stepCall{nativeBash("cat <dir>/notes.go")},
		},
		{ // 17: final dialog; rm -rf is clearWrite
			calls: []stepCall{nativeBash("rm -rf <dir>")},
		},
		{ // 18: closing summary with the measured run time; ends the turn
			think:   demoWrapUp,
			content: "", // filled by playStep from the recovered start time
		},
	}
}

// stepIndex derives the next script index from a request: the count of assistant
// messages already in context, which grows exactly one per completed script step.
func stepIndex(req chatRequest) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

// recoverRun scans prior assistant tool calls for the scratch path and returns the
// run it identifies. ok is false when no step has created one yet (step 0).
func recoverRun(req chatRequest) (*run, bool) {
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			name := tc.Function.Name
			var args string
			if len(tc.Function.Arguments) > 0 && tc.Function.Arguments[0] == '"' {
				_ = json.Unmarshal(tc.Function.Arguments, &args)
			} else {
				args = string(tc.Function.Arguments)
			}
			if name != "bash" || !strings.Contains(args, scratchPrefix) {
				continue
			}
			i := strings.LastIndex(args, "/tmp/ajent-demo-")
			suffix := args[i+len(scratchPrefix):]
			for j, r := range suffix {
				if r < '0' || r > '9' {
					suffix = suffix[:j]
					break
				}
			}
			ts, err := strconv.ParseInt(suffix, 10, 64)
			if err != nil || ts <= 0 {
				continue
			}
			return &run{dir: fmt.Sprintf("%s%d", scratchPrefix, ts), start: time.Unix(0, ts)}, true
		}
	}
	return nil, false
}

// advertisedNames lists the function names an agent advertises in its request.
func advertisedNames(req chatRequest) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		if n := strings.TrimSpace(t.Function.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// resolveCall picks the concrete tool call for a step against what an agent
// advertises: the native name when present (case-insensitive), else a bash-like
// tool with the command fallback. ok is false only when nothing can be resolved.
func resolveCall(advertised []string, c stepCall, r *run) (name string, args json.RawMessage, ok bool) {
	if slices.ContainsFunc(advertised, func(n string) bool { return strings.EqualFold(n, c.name) }) {
		return c.name, c.args(r), true
	}
	bsh := bashName(advertised)
	if bsh == "" || c.bash == "" {
		return "", nil, false // no native match and nothing useful to run via a shell
	}
	return bsh, jsonArgs(map[string]any{"command": expandBash(c.bash, r)}), true
}

// bashName returns the first advertised tool whose name contains bash or shell.
func bashName(advertised []string) string {
	for _, n := range advertised {
		l := strings.ToLower(n)
		if strings.Contains(l, "bash") || strings.Contains(l, "shell") {
			return n
		}
	}
	return ""
}

// expandBash substitutes the scratch dir into a bash fallback command.
func expandBash(cmd string, r *run) string {
	return strings.ReplaceAll(cmd, "<dir>", r.dir)
}

// jsonArgs encodes args as JSON; encoding/json never fails on these values.
func jsonArgs(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
