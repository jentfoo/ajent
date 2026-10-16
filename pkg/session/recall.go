package session

import (
	"time"

	"github.com/go-analyze/bulk"
)

// RecallIndex is the single source for editor Up/Down and Ctrl+R: every typed line
// from EditorHistory first, then recorded prompts that never reached the line file.
// The pasted-secret exclusion holds across both sources, so a line dropped from
// editor history cannot resurface through transcript backfill.
type RecallIndex struct {
	prompts *PromptIndex
	hist    *EditorHistory
}

// NewRecallIndex returns an index over store's recorded prompts merged with hist's
// typed lines for workspace.
func NewRecallIndex(s *Store, workspace string, hist *EditorHistory) *RecallIndex {
	return &RecallIndex{prompts: NewPromptIndex(s, workspace), hist: hist}
}

// Lines returns every recallable line for the workspace, newest first and
// deduplicated so each text keeps its most recent occurrence. A typed line that was
// also a recorded prompt carries the prompt's timestamp, otherwise At is zero.
func (r *RecallIndex) Lines() []Prompt {
	prompts := r.prompts.Prompts()
	// typed lines never carry the secret prefix, so only backfill needs this check
	if r.hist != nil {
		prefix := r.hist.secretPrefix
		prompts = bulk.SliceFilter(func(p Prompt) bool { return !hasSecretPrefix(prefix, p.Text) }, prompts)
	}
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
