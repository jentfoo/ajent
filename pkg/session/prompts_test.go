package session

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

func TestStorePrompts(t *testing.T) {
	// several cases pin the package clock via setClock, so this cannot run in parallel

	t.Run("recalls_newest_first_with_forks", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()

		w1, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		for _, txt := range []string{"first", "second", "third"} {
			_, aerr := w1.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, txt)})
			require.NoError(t, aerr)
		}
		// assistant and tool-result messages are not prompts
		_, aerr := w1.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "reply")})
		require.NoError(t, aerr)
		_, aerr = w1.Append(TypeMessage, MessageData{Message: llm.Message{Role: llm.RoleUser,
			Content: llm.BlockList{llm.ToolResultBlock{ToolName: "bash", Content: llm.BlockList{llm.TextBlock{Text: "out"}}}}}})
		require.NoError(t, aerr)

		fork := w1.Head()
		for _, txt := range []string{"forked prompt"} {
			_, ferr := w1.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, txt)})
			require.NoError(t, ferr)
		}
		w1.SetHead(fork) // rewind past the fork so only "third" is on the live branch
		require.NoError(t, w1.Close())

		prompts, perr := s.Prompts(ws)
		require.NoError(t, perr)

		texts := make([]string, 0, len(prompts))
		for _, p := range prompts {
			texts = append(texts, p.Text)
		}
		assert.Equal(t, []string{"forked prompt", "third", "second", "first"}, texts)
	})

	t.Run("dedupe_keeps_newest", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		t.Cleanup(setClock(time.UnixMilli(1_700_000_001).UTC()))
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		setClock(time.UnixMilli(1_800_000_002).UTC())
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "dup")})
		require.NoError(t, aerr)
		setClock(time.UnixMilli(1_900_000_003).UTC())
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "dup")})
		require.NoError(t, aerr)
		setClock(time.UnixMilli(2_100_000_004).UTC())
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "other")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())

		prompts, perr := s.Prompts(ws)
		require.NoError(t, perr)
		assert.Equal(t, []string{"other", "dup"}, PromptTexts(prompts))
	})

	t.Run("multi_line_joins_blocks", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "line one\nline two")})
		require.NoError(t, aerr)
		// multi-block content joins with newlines
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Message{Role: llm.RoleUser,
			Content: llm.BlockList{llm.TextBlock{Text: "a"}, llm.TextBlock{Text: "b"}}}})
		require.NoError(t, aerr)
		// blank-only text is skipped
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Message{Role: llm.RoleUser,
			Content: llm.BlockList{llm.TextBlock{Text: "   "}}}})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())

		prompts, perr := s.Prompts(ws)
		require.NoError(t, perr)
		assert.Equal(t, []string{"a\nb", "line one\nline two"}, PromptTexts(prompts))
	})

	// system-injected context (sub-agent completion steers, permission notes) never surfaces as a recallable prompt.
	t.Run("skips_injected", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "a real prompt")})
		require.NoError(t, aerr)
		// an injected steer carries user text but must stay out of recall
		_, ierr := w.Append(TypeMessage, MessageData{
			Message:  llm.Text(llm.RoleUser, "Sub-agent sub-2 completed. Call agent_poll with id sub-2."),
			Injected: true,
		})
		require.NoError(t, ierr)
		require.NoError(t, w.Close())

		prompts, perr := s.Prompts(ws)
		require.NoError(t, perr)
		assert.Equal(t, []string{"a real prompt"}, PromptTexts(prompts))
	})

	t.Run("limit", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		for i := 0; i < promptLimit+10; i++ {
			_, lerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, fmt.Sprintf("p%d", i))})
			require.NoError(t, lerr)
		}
		require.NoError(t, w.Close())

		prompts, perr := s.Prompts(ws)
		require.NoError(t, perr)
		assert.Len(t, prompts, promptLimit) // capped at the limit
	})

	t.Run("scan_files_cap", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		t.Cleanup(setClock(time.UnixMilli(1_700_000_001).UTC()))
		for i := 0; i < promptScanFiles+5; i++ {
			setClock(time.UnixMilli(int64(2_100_000_002 + i)))
			w, err := s.Create(ws, SessionData{Version: sessionVersion})
			require.NoError(t, err)
			_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, fmt.Sprintf("f%d", i))})
			require.NoError(t, aerr)
			require.NoError(t, w.Close())
		}
		prompts, perr := s.Prompts(ws)
		require.NoError(t, perr)
		assert.Len(t, prompts, promptScanFiles) // capped at the newest file count
	})

	t.Run("missing_dir_is_empty", func(t *testing.T) {
		s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
		prompts, err := s.Prompts("anywhere")
		require.NoError(t, err)
		assert.Empty(t, prompts)
	})
}

func TestPromptIndexPrompts(t *testing.T) {
	// both cases pin package clocks, so this cannot run in parallel

	t.Run("ttl", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		dir, err := s.Dir(ws)
		require.NoError(t, err)
		t.Cleanup(setClock(time.UnixMilli(1_000_001).UTC()))
		t.Cleanup(setPromptFollowsClock()) // promptNow tracks the mocked clock

		// explicit timestamped names so file order is deterministic across a refresh
		w, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-a.jsonl"), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		setClock(time.UnixMilli(1_000_001).UTC()) // first scan sets expires 5s later
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "one")})
		require.NoError(t, aerr)

		pIdx := NewPromptIndex(s, ws, "")
		assert.Equal(t, []string{"one"}, PromptTexts(pIdx.Prompts()))

		// inside the TTL an appended file does not trigger a rescan
		setClock(time.UnixMilli(1_003_000).UTC()) // within 5s of the first scan
		w2, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-b.jsonl"), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr = w2.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "two")})
		require.NoError(t, aerr)
		assert.Equal(t, []string{"one"}, PromptTexts(pIdx.Prompts()))

		// past the TTL a scan refreshes
		setClock(time.UnixMilli(1_010_000).UTC()) // well beyond expires (1006002)
		assert.Equal(t, []string{"two", "one"}, PromptTexts(pIdx.Prompts()))
	})

	t.Run("bump_refreshes", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		dir, err := s.Dir(ws)
		require.NoError(t, err)

		// explicit timestamped names so file order is deterministic across a refresh
		w, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-a.jsonl"), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "one")})
		require.NoError(t, aerr)

		pIdx := NewPromptIndex(s, ws, "") // default now hook, first scan sets a real-time expiry
		assert.Equal(t, []string{"one"}, PromptTexts(pIdx.Prompts()))

		// an older file is added after the cache warmed, then a now bump refreshes
		w2, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-b.jsonl"), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr = w2.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "two")})
		require.NoError(t, aerr)

		t.Cleanup(setPromptNow(time.UnixMilli(9_900_000_000_003).UTC())) // ~year 2300, past the first scan's real-time expiry
		assert.Equal(t, []string{"two", "one"}, PromptTexts(pIdx.Prompts()))
	})
	t.Run("secret_prefix_excluded", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		recordPrompt(t, s, ws, "secret:hunter2")
		recordPrompt(t, s, ws, "plain prompt")

		// the transcript keeps recording secret prompts (the model sees them), but
		// recall must not offer them back
		idx := NewPromptIndex(s, ws, "secret:")
		assert.Equal(t, []string{"plain prompt"}, PromptTexts(idx.Prompts()))
	})
}

func TestTranscriptIndexPrompts(t *testing.T) {
	t.Parallel()

	t.Run("recalls_newest_first_across_forks", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		w, err := s.Create(t.TempDir(), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		for _, txt := range []string{"first", "second", "third"} {
			_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, txt)})
			require.NoError(t, aerr)
		}
		// the fork lives in the file even though HEAD moved back, so every branch surfaces
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "forked prompt")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())

		idx := NewTranscriptIndex(w.Path(), "")
		assert.Equal(t, []string{"forked prompt", "third", "second", "first"}, PromptTexts(idx.Prompts()))
	})

	t.Run("append_refreshes", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		w, err := s.Create(t.TempDir(), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "one")})
		require.NoError(t, aerr)

		idx := NewTranscriptIndex(w.Path(), "")
		assert.Equal(t, []string{"one"}, PromptTexts(idx.Prompts()))

		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "two")})
		require.NoError(t, aerr)
		assert.Equal(t, []string{"two", "one"}, PromptTexts(idx.Prompts()))
	})

	t.Run("missing_file_keeps_last", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		idx := NewTranscriptIndex(path, "")
		assert.Empty(t, PromptTexts(idx.Prompts())) // never scanned, nothing to keep

		w, err := Create(path, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "kept")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())
		assert.Equal(t, []string{"kept"}, PromptTexts(idx.Prompts()))

		require.NoError(t, os.Remove(path))
		assert.Equal(t, []string{"kept"}, PromptTexts(idx.Prompts())) // a failed stat keeps the last list
	})

	t.Run("secret_prefix_excluded", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		w, err := s.Create(t.TempDir(), SessionData{Version: sessionVersion})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "secret:hunter2")})
		require.NoError(t, aerr)
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "plain prompt")})
		require.NoError(t, aerr)

		// the open session's own tree is what plain ↑/↓ serves, so its index must
		// drop secret prompts like every other recall surface
		idx := NewTranscriptIndex(w.Path(), "secret:")
		assert.Equal(t, []string{"plain prompt"}, PromptTexts(idx.Prompts()))

		// a live append rescans and keeps filtering
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "secret:more")})
		require.NoError(t, aerr)
		assert.Equal(t, []string{"plain prompt"}, PromptTexts(idx.Prompts()))
		t.Cleanup(func() { _ = w.Close() })
	})
}

// setPromptNow pins the package now hook to t for TTL determinism.
func setPromptNow(t time.Time) func() {
	old := promptNow
	promptNow = func() time.Time { return t }
	return func() { promptNow = old }
}

// setPromptFollowsClock pins the package now hook to whatever clock reports, so a
// TTL test can advance both together.
func setPromptFollowsClock() func() {
	old := promptNow
	promptNow = func() time.Time { return clock().UTC() }
	return func() { promptNow = old }
}

func TestStorePromptsMatchesSerial(t *testing.T) {
	// pins the shared scanWorkers var, so this is sequential
	pinScanWorkers(t, 2) // three files scan as 2+1, exercising the chunk boundary

	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	base := time.UnixMilli(1_700_000_000).UTC()
	for i := range 3 { // one file per scan chunk, newest last
		restore := setClock(base.Add(time.Duration(i) * time.Second))
		resetIDCounter()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		for j := range 2 {
			_, aerr := w.Append(TypeMessage, MessageData{
				Message:  llm.Text(llm.RoleUser, fmt.Sprintf("f%d p%d", i, j)),
				Injected: j == 0, // injected context never recalls
			})
			require.NoError(t, aerr)
		}
		require.NoError(t, w.Close())
		restore()
	}

	got, err := s.Prompts(ws)
	require.NoError(t, err)

	// serial reference: the pre-parallel walk, newest file first
	dir, derr := s.Dir(ws)
	require.NoError(t, derr)
	des, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	var names []string
	for _, de := range des {
		if !de.IsDir() && strings.HasSuffix(de.Name(), ".jsonl") {
			names = append(names, de.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(b, a) })
	var serial []Prompt
	seen := make(map[string]struct{})
	for _, name := range names {
		for _, p := range filePrompts(filepath.Join(dir, name)) {
			if _, dup := seen[p.Text]; dup {
				continue
			}
			seen[p.Text] = struct{}{}
			serial = append(serial, p)
		}
	}

	assert.Equal(t, serial, got)
}
