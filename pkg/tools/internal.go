package tools

import (
	"encoding/json"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// decode unmarshals raw tool arguments into v, with model-facing errors from strutil.DecodeArgs.
func decode(raw json.RawMessage, v any) error {
	return strutil.DecodeArgs(raw, v)
}

// badArgs prefixes err with the shared "bad args" tool-error text.
func badArgs(err error) agent.ToolResult {
	return agent.ErrorToolResult("bad args: " + err.Error())
}

// decodeCall unmarshals call arguments into T. ok is false when decoding fails,
// with res carrying the model-facing error result.
func decodeCall[T any](call agent.ToolCall) (T, agent.ToolResult, bool) {
	var p T
	if err := decode(call.Input, &p); err != nil {
		return p, badArgs(err), false
	}
	return p, agent.ToolResult{}, true
}

// resolveArg resolves a call's path argument through policy, an empty one
// defaulting to the session cwd. ok is false when resolution fails, with res
// carrying the model-facing error result.
func resolveArg(policy PathPolicy, path string) (string, agent.ToolResult, bool) {
	if path == "" {
		path = "."
	}
	full, err := policy.Resolve(path)
	if err != nil {
		return "", agent.ErrorToolResult(err.Error()), false
	}
	return full, agent.ToolResult{}, true
}

// llmBlock wraps text as a single model-visible block.
func llmBlock(text string) llm.BlockList {
	return llm.BlockList{llm.TextBlock{Text: text}}
}

// fileInfo returns stat info for path, or nil when it cannot be read so Observe
// still records content without crashing on a disappearing file.
func fileInfo(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}
