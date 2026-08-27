package session

import (
	"encoding/json"
	"fmt"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// State rebuilds agent state from a branch, resolving model switches through
// resolve. A failure to resolve is a warning, never an error; the caller falls
// back to its registry's active model.
func State(branch []Entry, resolve func(key string) (llm.Model, error)) (agent.State, []string) {
	var st agent.State
	var warns []string
	st.Tokens = tokens.New(llm.Model{})

	cd, _, _ := NewestCompaction(branch)
	msgs, mwarns := ContextMessages(branch, cd, resolve)
	warns = append(warns, mwarns...)
	st.Messages = msgs

	// a compaction rewrites what the branch sends, so the prompt sizes recorded
	// against its surviving messages describe a request that no longer exists. When
	// one applies the context terms are measured from the assembled messages instead
	// (below) and recorded usage counts only toward spend.
	rewritten := cd.rewritesHistory()

	for i := range branch {
		e := branch[i]
		switch e.Type {
		case TypeMessage:
			var md MessageData
			if err := e.Decode(&md); err != nil {
				continue // already warned by ContextMessages; ledger just skips it
			}
			if rewritten {
				// spend counts every message, including ones the cut removed: those
				// tokens were billed whether or not they still occupy context
				st.Tokens.RecordSpend(st.Model.Key(), md.Usage)
				continue
			}
			rebuildUsage(st.Tokens, st.Model.Key(), md)
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
			st.Tokens.SetModel(resolved)
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
			st.Tokens.SetModel(resolved)
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

	if rewritten {
		// the summary message assembly prepends is not an entry, so measuring the
		// assembled list is also what accounts for it
		st.Tokens.Reseed(tokens.EstimateFor(st.Model, st.Reasoning.Retain, st.Messages))
	}

	if !wellFormed(st.Messages) {
		warns = append(warns, "rebuilt context leaves a tool call unanswered")
	}
	return st, warns
}

// NewestCompaction decodes the last compaction entry on the branch, returning its
// data and branch index. found is false when the branch carries none, where the
// zero value applies no cut and no reductions.
func NewestCompaction(branch []Entry) (cd CompactionData, idx int, found bool) {
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == TypeCompaction {
			_ = branch[i].Decode(&cd)
			return cd, i, true
		}
	}
	return CompactionData{}, -1, false
}

// rebuildUsage folds one message's recorded usage into the ledger under key. A
// provider report snaps the exact terms; later messages without one stay as an
// estimate so /usage reconciles with what was actually sent. It is only reached on
// a branch no compaction rewrote, where the recorded prompt is still what the next
// request will carry.
func rebuildUsage(t *tokens.Accounting, key string, md MessageData) {
	if t == nil {
		return
	}
	if tokens.Zero(md.Usage) { // unreported provider: estimate the message instead of exact terms
		t.Add(tokens.EstimateMessages([]llm.Message{md.Message}))
		return
	}
	// prediction unknown on rebuild, so calibration stays unseeded. keepThink is
	// true because the resolved retention of the turn that produced this usage is
	// not recorded; leaving the reported output whole is the conservative read.
	t.Response(key, md.Usage, 0, true)
}

// applySetting folds one setting_change into state. Unknown keys are ignored.
func applySetting(st *agent.State, key string, value json.RawMessage) {
	switch key {
	case "reasoning":
		var rc llm.ReasoningConfig
		if err := json.Unmarshal(value, &rc); err == nil {
			st.Reasoning = rc
		}
	case "tools", "tools.enabled": // dotted config key plus the legacy alias
		var tools []string
		if err := json.Unmarshal(value, &tools); err == nil {
			st.Tools = tools
		}
	}
}

// SettingOverrides returns the last written value per setting_change key on a
// branch, for seeding the session config layer when resuming.
func SettingOverrides(branch []Entry) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage)
	for _, e := range branch {
		if e.Type != TypeSettingChange {
			continue
		}
		var sd SettingData
		if err := e.Decode(&sd); err != nil || len(sd.Value) == 0 {
			continue
		}
		out[sd.Key] = sd.Value
	}
	return out
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
