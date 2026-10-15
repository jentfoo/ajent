package tools

import (
	"encoding/json"
	"os"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// decode unmarshals raw tool arguments into v, with model-facing errors from strutil.DecodeArgs.
func decode(raw json.RawMessage, v any) error {
	return strutil.DecodeArgs(raw, v)
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
