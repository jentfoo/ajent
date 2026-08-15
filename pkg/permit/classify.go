package permit

import (
	"encoding/json"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
)

// Verdict is the static outcome of classifying one tool call.
type Verdict uint8

const (
	VerdictAllow  Verdict = iota // verifiably read-only, runs without a prompt
	VerdictReject                // hard refusal with guidance; no dialog
	VerdictPrompt                // needs approval or model classification
)

// builtinReadOnly are core tools that only ever read. Registry.ReadOnly is false
// for them today (only MCP calls MarkReadOnly), so they match by name here.
var builtinReadOnly = bulk.SliceToSet([]string{"read", "grep", "find", "ls"})

// coreWriteTools always mutate and never auto-allow on declared metadata, even if
// a config glob or annotation were to mark them read-only by mistake.
var coreWriteTools = bulk.SliceToSet([]string{"write", "edit"})

// bashCommand extracts the command field from a bash tool call, returning empty
// when it cannot be decoded so classification fails safe to the prompt path.
func bashCommand(input json.RawMessage) string {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return ""
	}
	return p.Command
}

// Classify statically sorts a call into allow / reject / prompt using declared
// metadata and the shell analyser. Session allows, mode and the model classifier
// live above it; there is no name-prefix auto-approval.
func Classify(call agent.ToolCall, ro func(string) bool) Verdict {
	if _, ok := builtinReadOnly[call.Name]; ok {
		return VerdictAllow
	}
	// declared metadata only ever auto-allows non-built-in (MCP/extension) tools;
	// a core write tool prompts regardless of what the registry claims.
	_, isWrite := coreWriteTools[call.Name]
	if !isWrite && ro != nil && ro(call.Name) {
		return VerdictAllow // MCP readOnlyHint or config globs
	}
	if call.Name == bashTool {
		command := bashCommand(call.Input)
		if sedWrite(command) {
			return VerdictReject // in-place write; guidance to use the edit tool
		}
		if allSegmentsReadOnly(scanCommand(command)) {
			return VerdictAllow
		}
	}
	return VerdictPrompt
}
