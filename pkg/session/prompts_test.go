package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorePrompts(t *testing.T) {
	t.Parallel()

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
	assert.Equal(t, []string{"forked prompt", "third", "second", "first"}, texts,
		"newest first with the abandoned fork included")
}

func TestStorePromptsDedupeKeepsNewest(t *testing.T) {
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
	assert.Equal(t, []string{"other", "dup"}, promptTexts(prompts), "duplicate keeps only its newest occurrence")
}

func TestStorePromptsMultiLineJoinsBlocks(t *testing.T) {
	t.Parallel()

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
	assert.Equal(t, []string{"a\nb", "line one\nline two"}, promptTexts(prompts))
}

// TestStorePromptsSkipsInjected verifies system-injected context (sub-agent
// completion steers, permission notes) never surfaces as a recallable prompt.
func TestStorePromptsSkipsInjected(t *testing.T) {
	t.Parallel()

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
	assert.Equal(t, []string{"a real prompt"}, promptTexts(prompts),
		"injected context is not a recallable prompt")
}

func TestStorePromptsLimit(t *testing.T) {
	t.Parallel()

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
}

func TestStorePromptsScanFilesCap(t *testing.T) {
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
}

func TestStorePromptsMissingDirIsEmpty(t *testing.T) {
	t.Parallel()

	s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
	prompts, err := s.Prompts("anywhere")
	require.NoError(t, err)
	assert.Empty(t, prompts)
}

func TestPromptIndexPromptsTTL(t *testing.T) {
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

	pIdx := NewPromptIndex(s, ws)
	assert.Equal(t, []string{"one"}, promptTexts(pIdx.Prompts()))

	// inside the TTL an appended file does not trigger a rescan
	setClock(time.UnixMilli(1_003_000).UTC()) // within 5s of the first scan
	w2, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-b.jsonl"), SessionData{Version: sessionVersion})
	require.NoError(t, err)
	_, aerr = w2.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "two")})
	require.NoError(t, aerr)
	assert.Equal(t, []string{"one"}, promptTexts(pIdx.Prompts()), "cached within the TTL")

	// past the TTL a scan refreshes
	setClock(time.UnixMilli(1_010_000).UTC()) // well beyond expires (1006002)
	assert.Equal(t, []string{"two", "one"}, promptTexts(pIdx.Prompts()))
}

func TestPromptIndexPromptsBumpRefreshes(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	dir, err := s.Dir(ws)
	require.NoError(t, err)

	// explicit timestamped names so file order is deterministic across a refresh
	w, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-a.jsonl"), SessionData{Version: sessionVersion})
	require.NoError(t, err)
	_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "one")})
	require.NoError(t, aerr)

	pIdx := NewPromptIndex(s, ws) // default now hook; first scan sets a real-time expiry
	assert.Equal(t, []string{"one"}, promptTexts(pIdx.Prompts()))

	// an older file is added after the cache warmed, then a now bump refreshes
	w2, err := Create(filepath.Join(dir, "2026-01-02T00-00-00Z-b.jsonl"), SessionData{Version: sessionVersion})
	require.NoError(t, err)
	_, aerr = w2.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "two")})
	require.NoError(t, aerr)

	t.Cleanup(setPromptNow(time.UnixMilli(9_900_000_000_003).UTC())) // ~year 2300, past the first scan's real-time expiry
	assert.Equal(t, []string{"two", "one"}, promptTexts(pIdx.Prompts()), "a now bump refreshes")
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

// promptTexts returns each prompt's text in order.
func promptTexts(ps []Prompt) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Text
	}
	return out
}
