package session

import (
	"encoding/json"
	"fmt"

	"github.com/go-analyze/bulk"
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

	cd := newestCompactionData(branch)
	msgs, mwarns := ContextMessages(branch, cd, resolve)
	warns = append(warns, mwarns...)
	st.Messages = msgs

	// the ledger's context terms must reflect only messages that survive compaction:
	// entries summarized away by a cut or dropped outright are gone from what the next
	// request sends. Cumulative spend (Accounting.Total) is unaffected; it comes from
	// recorded usage, not this rebuild.
	keepIdx := cutIndex(branch, cd)
	var dropped map[string]struct{}
	if r := cd.Reduce; r != nil {
		dropped = bulk.SliceToSet(r.Drop)
	}

	for i := range branch {
		e := branch[i]
		switch e.Type {
		case TypeMessage:
			if keepIdx != 0 && (keepIdx < 0 || i < keepIdx) { // cut away or cut missing
				continue
			}
			if _, ok := dropped[e.ID]; ok {
				continue
			}
			var md MessageData
			if err := e.Decode(&md); err != nil {
				continue // already warned by ContextMessages; ledger just skips it
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

	if !wellFormed(st.Messages) {
		warns = append(warns, "rebuilt context leaves a tool call unanswered")
	}
	return st, warns
}

// newestCompactionData decodes the last compaction entry on the branch. When none
// exists it returns an empty value so assembly applies no cut and no reductions.
func newestCompactionData(branch []Entry) CompactionData {
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == TypeCompaction {
			var cd CompactionData
			_ = branch[i].Decode(&cd)
			return cd
		}
	}
	return CompactionData{}
}

// rebuildUsage folds one message's recorded usage into the ledger under key. A
// provider report snaps the exact terms; later messages without one stay as an
// estimate so /usage reconciles with what was actually sent.
func rebuildUsage(t *tokens.Accounting, key string, md MessageData) {
	if t == nil {
		return
	}
	if tokens.Zero(md.Usage) { // unreported provider: estimate the message instead of exact terms
		t.Add(tokens.EstimateMessages([]llm.Message{md.Message}))
		return
	}
	t.Response(key, md.Usage, 0) // prediction unknown on rebuild; leave calibration unseeded
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
