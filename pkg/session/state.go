package session

import (
	"encoding/json"
	"fmt"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// State rebuilds agent state from a branch, resolving model switches through
// resolve. A failure to resolve is a warning, never an error — the caller falls
// back to its registry's active model.
func State(branch []Entry, resolve func(key string) (llm.Model, error)) (agent.State, []string) {
	var st agent.State
	var warns []string

	// the newest compaction collapses earlier messages into one summary
	var keepIdx int
	if pos := newestCompaction(branch); pos >= 0 {
		var cd CompactionData
		if err := branch[pos].Decode(&cd); err != nil {
			warns = append(warns, "invalid compaction entry: "+err.Error())
		} else if p, ok := indexOf(branch, cd.FirstKeptEntryID); ok {
			keepIdx = p
			st.Messages = append(st.Messages, llm.Message{
				Role:    llm.RoleSystem,
				Content: llm.BlockList{llm.TextBlock{Text: summaryNote(cd.Summary)}},
			})
		} else {
			warns = append(warns, "compaction first-kept entry not found")
		}
	}

	for i := range branch {
		e := branch[i]
		switch e.Type {
		case TypeMessage:
			if i < keepIdx {
				continue // summarized away by the newest compaction
			}
			var md MessageData
			if err := e.Decode(&md); err != nil {
				warns = append(warns, "invalid message entry: "+err.Error())
				continue
			}
			st.Messages = append(st.Messages, md.Message)
		case TypeModelChange:
			var m ModelData
			if err := e.Decode(&m); err != nil {
				warns = append(warns, "invalid model_change entry: "+err.Error())
				continue
			}
			resolved, rerr := resolve(m.Model)
			if rerr != nil {
				warns = append(warns, fmt.Sprintf("unresolved model key %q", m.Model))
				continue // keep the previous model on failure
			}
			st.Model = resolved
		case TypeSession:
			// seed the active model from session start; a later model_change overrides it.
			var sd SessionData
			if err := e.Decode(&sd); err != nil || sd.Model == "" {
				continue
			}
			resolved, rerr := resolve(sd.Model)
			if rerr != nil {
				warns = append(warns, fmt.Sprintf("unresolved session model key %q", sd.Model))
				continue
			}
			st.Model = resolved
		case TypeSettingChange:
			var sd SettingData
			if err := e.Decode(&sd); err != nil {
				warns = append(warns, "invalid setting_change entry: "+err.Error())
				continue
			}
			applySetting(&st, sd.Key, sd.Value)
		default:
			// notice / custom entries do not shape state
		}
	}

	if !wellFormed(st.Messages) {
		warns = append(warns, "rebuilt context leaves a tool call unanswered")
	}
	return st, warns
}

// newestCompaction returns the index of the last compaction entry, or -1.
func newestCompaction(branch []Entry) int {
	idx := -1
	for i := range branch {
		if branch[i].Type == TypeCompaction {
			idx = i
		}
	}
	return idx
}

// indexOf returns the file-order position of id.
func indexOf(branch []Entry, id string) (int, bool) {
	for i := range branch {
		if branch[i].ID == id {
			return i, true
		}
	}
	return 0, false
}

func summaryNote(s string) string { return "Summary of earlier conversation:\n\n" + s }

// applySetting folds one setting_change into state. Unknown keys are ignored.
func applySetting(st *agent.State, key string, value json.RawMessage) {
	switch key {
	case "reasoning":
		var rc llm.ReasoningConfig
		if err := json.Unmarshal(value, &rc); err == nil {
			st.Reasoning = rc
		}
	case "tools":
		var tools []string
		if err := json.Unmarshal(value, &tools); err == nil {
			st.Tools = tools
		}
	}
}

// wellFormed reports whether every ToolCallBlock has a matching ToolResultBlock,
// which is what keeps the next request valid.
func wellFormed(msgs []llm.Message) bool {
	calls := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			switch blk := b.(type) {
			case llm.ToolCallBlock:
				calls++
			case llm.ToolResultBlock:
				if blk.CallID == "" || calls <= 0 {
					return false
				}
				calls--
			}
		}
	}
	return calls == 0
}
