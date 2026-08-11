package tools

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// decode unmarshals raw into v, returning an error on malformed input so a tool
// degrades to an error result rather than panicking.
func decode(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("malformed arguments: %w", err)
	}
	return nil
}

// llmBlock wraps text as a single model-visible block.
func llmBlock(text string) llm.BlockList {
	return llm.BlockList{llm.TextBlock{Text: text}}
}

// resultErr builds an error ToolResult carrying the message the model should see.
func resultErr(msg string) agent.ToolResult {
	return agent.ToolResult{Content: llmBlock(msg), IsError: true}
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

// discard is an Output that drops everything, used when no sink is attached so a
// tool never dereferences a nil interface.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Diff(string, string, string) {}

// ensureOutput returns out or a discarding Output when it is nil.
func ensureOutput(out agent.Output) agent.Output {
	if out == nil {
		return discard{}
	}
	return out
}
