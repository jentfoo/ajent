package session

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/llm"
)

const (
	promptScanFiles = 100 // newest transcript files scanned per workspace
	promptLimit     = 500 // prompts offered to history search at most
	promptTTL       = 5 * time.Second
)

// promptNow supplies timestamps so the PromptIndex TTL is testable without sleeping.
var promptNow = func() time.Time { return time.Now().UTC() }

// Prompt is one recallable line offered to history search. At may be zero when
// the line came from editor-history rather than a recorded transcript.
type Prompt struct {
	Text string
	At   time.Time
}

// PromptTexts returns each prompt's text in order.
func PromptTexts(ps []Prompt) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Text
	}
	return out
}

// Prompts returns the workspace's recorded user prompts, newest first and
// deduplicated so each distinct text keeps only its most recent occurrence. A
// directory with no sessions yields an empty slice without error.
func (s *Store) Prompts(workspace string) ([]Prompt, error) {
	dir, err := s.Dir(workspace)
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no sessions yet is not an error
		}
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
			names = append(names, f.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(b, a) }) // newest file first
	if len(names) > promptScanFiles {
		names = names[:promptScanFiles]
	}

	out := make([]Prompt, 0, len(names))
	seen := make(map[string]struct{})
	// newest-first batches keep the quota honest: once out is full the remaining
	// files stay unread, and each transcript parses concurrently. The clamp
	// mirrors scanEach's, so a pinned zero never stalls either side.
	for chunk := range slices.Chunk(names, max(scanWorkers, 1)) {
		paths := make([]string, len(chunk))
		for i, name := range chunk {
			paths[i] = filepath.Join(dir, name)
		}
		results := scanEach(paths, func(p string) []Prompt { return filePrompts(p) })
		for _, prompts := range results { // file order within the batch is name order
			for i := 0; i < len(prompts) && len(out) < promptLimit; i++ { // keep most recent occurrence
				if _, dup := seen[prompts[i].Text]; dup {
					continue
				}
				seen[prompts[i].Text] = struct{}{}
				out = append(out, prompts[i])
			}
			if len(out) >= promptLimit {
				return out, nil
			}
		}
	}
	return out, nil
}

// filePrompts returns one transcript's recorded user prompts, newest first.
func filePrompts(path string) []Prompt {
	entries, _, err := Read(path)
	if err != nil {
		return nil
	}
	var prompts []Prompt
	for i := len(entries) - 1; i >= 0; i-- { // append-only, reverse is newest first
		e := entries[i]
		if e.Type != TypeMessage {
			continue
		}
		var md MessageData
		if err := e.Decode(&md); err != nil {
			continue
		}
		if md.Message.Role != llm.RoleUser || md.Injected {
			continue // assistant/tool messages and system-injected context are not prompts
		}
		txt := llm.JoinText(md.Message.Content)
		if txt == "" {
			continue
		}
		prompts = append(prompts, Prompt{Text: txt, At: time.UnixMilli(e.TS).UTC()})
	}
	return prompts
}

// dropSecretPrompts removes prompts carrying the pasted-secret prefix, so a
// transcript-derived surface cannot resurface what editor history never stored.
func dropSecretPrompts(prefix string, ps []Prompt) []Prompt {
	return bulk.SliceFilter(func(p Prompt) bool { return !HasSecretPrefix(prefix, p.Text) }, ps)
}

// PromptIndex caches a workspace's prompt list on a short TTL so Ctrl+R never
// rescans every transcript per keystroke.
type PromptIndex struct {
	store        *Store
	workspace    string
	secretPrefix string

	mu      sync.Mutex
	prompts []Prompt
	expires time.Time
}

// NewPromptIndex returns an index over store's prompts for workspace, excluding
// secret-prefixed ones.
func NewPromptIndex(store *Store, workspace, secretPrefix string) *PromptIndex {
	return &PromptIndex{store: store, workspace: workspace, secretPrefix: secretPrefix}
}

// Prompts returns the cached prompt list, rescanning when it has expired. It is
// best effort: a scan failure yields an empty slice rather than an error.
func (p *PromptIndex) Prompts() []Prompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !promptNow().Before(p.expires) || p.prompts == nil {
		ps, _ := p.store.Prompts(p.workspace)
		p.prompts = dropSecretPrompts(p.secretPrefix, ps)
		p.expires = promptNow().Add(promptTTL)
	}
	return p.prompts
}

// TranscriptIndex serves one transcript's recorded prompts, re-read only when
// the file changes.
type TranscriptIndex struct {
	path         string
	secretPrefix string

	mu      sync.Mutex
	scanned bool
	size    int64
	mod     time.Time
	prompts []Prompt
}

// NewTranscriptIndex returns an index over the transcript at path, excluding
// secret-prefixed prompts.
func NewTranscriptIndex(path, secretPrefix string) *TranscriptIndex {
	return &TranscriptIndex{path: path, secretPrefix: secretPrefix}
}

// Prompts returns the transcript's recorded user prompts, newest first across
// every branch of its tree. Secret-prefixed ones stay recorded but are never
// offered for recall. Unchanged size and mtime reuse the last read; a failed
// stat or read keeps the last good list.
func (p *TranscriptIndex) Prompts() []Prompt {
	p.mu.Lock()
	defer p.mu.Unlock()

	fi, err := os.Stat(p.path)
	if err != nil {
		return p.prompts
	}
	if !p.scanned || fi.Size() != p.size || !fi.ModTime().Equal(p.mod) {
		p.prompts = dropSecretPrompts(p.secretPrefix, filePrompts(p.path))
		p.scanned, p.size, p.mod = true, fi.Size(), fi.ModTime()
	}
	return p.prompts
}
