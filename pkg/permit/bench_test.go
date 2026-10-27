package permit

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/require"
)

// benchBash builds a bash call carrying command.
func benchBash(command string) agent.ToolCall {
	raw, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		panic(err)
	}
	return agent.ToolCall{ID: "c", Name: "bash", Input: raw}
}

// benchCommandTable is a realistic spread: read-only git forms including the
// branch/tag/config duals, a network git verb, compound pipes, a compound
// write, redirects and .git paths. Classify must split allow from prompt
// across all of it.
func benchCommandTable() []agent.ToolCall {
	return []agent.ToolCall{
		benchBash("git status --short"),
		benchBash("git log --oneline -20"),
		benchBash("git diff HEAD~1 -- parser.go"),
		benchBash("git show abc123 --stat"),
		benchBash("git branch --list 'feat/*'"),
		benchBash("git tag -l v1.*"),
		benchBash("git config --get user.email"),
		benchBash("git config user.name"),
		benchBash("git remote -v"),
		benchBash("git push origin main"),
		benchBash("git add -A && git commit -m 'wip'"),
		benchBash("cat internal/parser/parse.go | grep -n panic | wc -l"),
		benchBash("rg --files -g '*.go' | head -50"),
		benchBash("for f in *.md; do head -20 \"$f\"; done"),
		benchBash("for f in *.tmp; do rm \"$f\"; done"),
		benchBash("echo notes > /tmp/notes.txt"),
		benchBash("curl -s https://example.com | jq ."),
		benchBash("sed -i 's/a/b/' file.go"),
		// the analyser's hard shapes: substitution nesting, env indirection and
		// pipelines deep enough that the head walk has to reach the last stage
		benchBash("git -C $(dirname \"$PWD/repo\") log --oneline -5"),
		benchBash("cat `which go` | wc -c"),
		benchBash("LC_ALL=C git status --short && echo $(date +%s)"),
		benchBash("find . -name '*.go' | xargs grep -l panic | sort | head -20"),
		benchBash("awk '{print $1}' access.log | uniq -c | sort -rn | head -5"),
		benchBash("(cd pkg/parser && go test ./...) 2>&1 | tail -30"),
		benchBash("env GIT_PAGER=cat git diff --stat HEAD~3"),
	}
}

// benchScopeTable is the write-spread the auto+write scope resolves: in-scope
// writes, escapes outside the roots, and VCS metadata paths that are never
// unattended.
func benchScopeTable(cwd string) []agent.ToolCall {
	mk := func(name, path string) agent.ToolCall {
		return agent.ToolCall{ID: "c", Name: name,
			Input: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))}
	}
	return []agent.ToolCall{
		mk("edit", "internal/parser/parse.go"),
		mk("write", "notes.md"),
		mk("edit", "../outside.txt"),
		mk("write", ".git/hooks/pre-commit"),
		mk("edit", "$HOME/escape.txt"),
		benchBash("cd sub && make generate"),
		benchBash("cd ../.. && ls"),
	}
}

// BenchmarkClassify measures the static gate every approved tool call runs:
// Classify over the command table, the dialog subject render, the compound
// head walk behind session grants, and auto+write scope resolution.
func BenchmarkClassify(b *testing.B) {
	calls := benchCommandTable()
	ro := func(string) bool { return false } // no declared read-only metadata

	// verdicts must spread; a table that all lands one way proves nothing
	var counts [3]int
	for _, call := range calls {
		counts[Classify(call, ro)]++
	}
	require.Positive(b, counts[VerdictAllow])
	require.Positive(b, counts[VerdictPrompt])

	b.Run("classify", func(b *testing.B) {
		for b.Loop() {
			for _, call := range calls {
				Classify(call, ro)
			}
		}
	})
	b.Run("subject", func(b *testing.B) {
		for b.Loop() {
			for _, call := range calls {
				_ = elideSubject(subjectFor(call))
			}
		}
	})
	b.Run("governing_heads", func(b *testing.B) {
		var compounds []string
		for _, call := range calls {
			if cmd := subjectFor(call); strings.ContainsAny(cmd, "|&;$") {
				compounds = append(compounds, cmd)
			}
		}
		require.NotEmpty(b, compounds)

		for b.Loop() {
			for _, cmd := range compounds {
				_, _ = compoundGoverningHeads(cmd)
			}
		}
	})
	b.Run("write_scope", func(b *testing.B) {
		cwd := b.TempDir()
		scope := newWriteScope(cwd)
		calls := benchScopeTable(cwd)
		for b.Loop() {
			for _, call := range calls {
				_ = scope.allows(call)
			}
		}
	})
}
