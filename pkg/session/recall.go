package session

import (
	"time"
)

// RecallIndex is the single source for editor Up/Down and Ctrl+R: every typed line
// from EditorHistory first, then recorded prompts that never reached the line file.
// Typed lines drop secret-prefixed input at append time and the prompt index drops
// it at read time, so a hidden line cannot resurface through either source.
type RecallIndex struct {
	prompts *PromptIndex
	hist    *EditorHistory
}

// NewRecallIndex returns an index over store's recorded prompts merged with hist's
// typed lines for workspace. secretPrefix is the pasted-secret marker excluded from
// what recall offers; a nil hist yields no typed lines.
func NewRecallIndex(s *Store, workspace, secretPrefix string, hist *EditorHistory) *RecallIndex {
	return &RecallIndex{prompts: NewPromptIndex(s, workspace, secretPrefix), hist: hist}
}

// Lines returns every recallable line for the workspace, newest first and
// deduplicated so each text keeps its most recent occurrence. A typed line that was
// also a recorded prompt carries the prompt's timestamp, otherwise At is zero.
func (r *RecallIndex) Lines() []Prompt {
	prompts := r.prompts.Prompts()
	at := make(map[string]time.Time, len(prompts))
	for _, p := range prompts { // keep each text's newest recorded time
		if _, ok := at[p.Text]; !ok {
			at[p.Text] = p.At
		}
	}

	var recent []string
	if r.hist != nil { // typed lines are the complete current record, list them first
		recent = r.hist.Recent()
	}
	out := make([]Prompt, 0, len(recent)+len(prompts))
	seen := make(map[string]struct{}, len(recent)+len(prompts))
	addTyped := func(txt string) {
		out = append(out, Prompt{Text: txt, At: at[txt]}) // zero time when never recorded
		seen[txt] = struct{}{}
	}
	for _, txt := range recent {
		addTyped(txt)
	}
	for _, p := range prompts { // backfill older transcripts not yet in the line file
		if _, ok := seen[p.Text]; ok {
			continue
		}
		out = append(out, p)
		seen[p.Text] = struct{}{}
	}
	return out
}
