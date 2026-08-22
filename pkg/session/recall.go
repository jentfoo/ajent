package session

import "time"

// RecallIndex is the single source for editor Up/Down and Ctrl+R: every typed line
// from EditorHistory first, then recorded prompts that never reached the line file
// (e.g. transcripts written before this phase). Newest first, deduped by text.
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
// also a recorded prompt carries the prompt's timestamp; otherwise At is zero.
func (r *RecallIndex) Lines() []Prompt {
	prompts := r.prompts.Prompts()
	at := make(map[string]time.Time, len(prompts))
	for _, p := range prompts { // keep each text's newest recorded time
		if _, ok := at[p.Text]; !ok {
			at[p.Text] = p.At
		}
	}

	var out []Prompt
	seen := make(map[string]struct{})
	addTyped := func(txt string) {
		out = append(out, Prompt{Text: txt, At: at[txt]}) // zero time when never recorded
		seen[txt] = struct{}{}
	}
	if r.hist != nil { // typed lines are the complete current record; list them first
		for _, txt := range r.hist.Recent() {
			addTyped(txt)
		}
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
