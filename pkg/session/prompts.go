package session

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
)

const (
	promptScanFiles = 100 // newest transcript files scanned per workspace
	promptLimit     = 500 // prompts offered to history search at most
	promptTTL       = 5 * time.Second
)

// promptNow supplies timestamps so the PromptIndex TTL is testable without sleeping.
var promptNow = func() time.Time { return time.Now().UTC() }

// Prompt is one recorded user prompt offered to history search.
type Prompt struct {
	Text string
	At   time.Time
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
	var names []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
			names = append(names, f.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(b, a) }) // newest file first
	if len(names) > promptScanFiles {
		names = names[:promptScanFiles]
	}

	var out []Prompt
	seen := make(map[string]struct{})
	for _, name := range names {
		prompts := filePrompts(filepath.Join(dir, name))              // newest first within the file
		for i := 0; i < len(prompts) && len(out) < promptLimit; i++ { // keep most recent occurrence
			if _, dup := seen[prompts[i].Text]; dup {
				continue
			}
			seen[prompts[i].Text] = struct{}{}
			out = append(out, prompts[i])
		}
		if len(out) >= promptLimit {
			break
		}
	}
	return out, nil
}

// filePrompts returns one transcript's user text blocks in file order.
func filePrompts(path string) []Prompt {
	entries, _, err := Read(path)
	if err != nil {
		return nil
	}
	var prompts []Prompt
	for i := len(entries) - 1; i >= 0; i-- { // append-only: reverse is newest first
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
		txt := messageText(md)
		if txt == "" {
			continue
		}
		prompts = append(prompts, Prompt{Text: txt, At: time.UnixMilli(e.TS).UTC()})
	}
	return prompts
}

// PromptIndex caches a workspace's prompt list on a short TTL so Ctrl+R never
// rescans every transcript per keystroke.
type PromptIndex struct {
	store     *Store
	workspace string

	mu      sync.Mutex
	prompts []Prompt
	expires time.Time
}

// NewPromptIndex returns an index over store's prompts for workspace.
func NewPromptIndex(store *Store, workspace string) *PromptIndex {
	return &PromptIndex{store: store, workspace: workspace}
}

// Prompts returns the cached prompt list, rescanning when it has expired. It is
// best effort: a scan failure yields an empty slice rather than an error.
func (p *PromptIndex) Prompts() []Prompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !promptNow().Before(p.expires) || p.prompts == nil {
		ps, _ := p.store.Prompts(p.workspace)
		p.prompts = ps
		p.expires = promptNow().Add(promptTTL)
	}
	return p.prompts
}
