package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

// The benchmarks here generate one multi-branch transcript with a compaction
// in the store, then measure the resume replay: Read, Branch and
// ContextMessages, plus the store's List and Prompts scans.

const benchTurns = 90 // tool-calling turns before the cut, ~350 entries with the fork

// writeBenchTranscript generates the fixture at path: session entry,
// benchTurns tool-calling turns, one compaction entry with a cut and reduce
// plan, more turns after, then a rewind that forks a short branch ending as
// the persisted head. Returns the compaction entry's data.
func writeBenchTranscript(b *testing.B, path string) CompactionData {
	b.Helper()

	w, err := Create(path, SessionData{Version: Version(), Workspace: path})
	require.NoError(b, err)

	ask := func(text string) Entry {
		e, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, text)})
		require.NoError(b, aerr)
		return e
	}
	turn := func(i int) string {
		callID := fmt.Sprintf("call_%04d", i)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Message{
			Role: llm.RoleAssistant, Content: llm.BlockList{
				llm.ThinkingBlock{Text: fmt.Sprintf("plan turn %d: trace the trim boundary", i),
					Signature: fmt.Sprintf("sig-%04d", i)},
				llm.TextBlock{Text: fmt.Sprintf("checking the reducer, pass %d", i)},
				llm.ToolCallBlock{ID: callID, Name: "bash",
					Input: json.RawMessage(fmt.Sprintf(`{"command":"go test ./reduce -run Test%d"}`, i))},
			}, Stop: llm.StopToolUse}})
		require.NoError(b, aerr)
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Message{
			Role: llm.RoleUser, Content: llm.BlockList{
				llm.ToolResultBlock{CallID: callID, ToolName: "bash",
					Content: llm.BlockList{llm.TextBlock{
						Text: strings.Repeat(fmt.Sprintf("line %d of captured parser output\n", i), 24)}}}}}})
		require.NoError(b, aerr)
		return callID
	}

	// history the cut summarizes away
	for i := range benchTurns / 2 {
		ask(fmt.Sprintf("investigate pass %d", i))
		turn(i)
	}

	// the kept tail, with a few results the reduce plan stubs
	var stubs []Stub
	keptStart := ask("now fix the trim boundary")
	for i := benchTurns / 2; i < benchTurns; i++ {
		if callID := turn(i); len(stubs) < 8 {
			stubs = append(stubs, Stub{CallID: callID, Text: "(elided: superseded run)"})
		}
	}

	// compaction entry, replayed by ContextMessages on every rebuild
	cd := CompactionData{
		Summary:          strings.Repeat("the parser work so far, condensed. ", 10),
		FirstKeptEntryID: keptStart.ID,
		Before:           48000,
		After:            21000,
		Reduce:           &Reduce{Stubs: stubs, StripThinking: true, Stats: Stats{Summarized: benchTurns / 2}},
	}
	_, aerr := w.Append(TypeCompaction, cd)
	require.NoError(b, aerr)

	// post-compaction turns the cut keeps verbatim, then a rewind mid-tail that
	// forks: both branches stay in the file, the fork tip becomes the persisted
	// head resume continues from, and its ancestry carries the compaction entry
	var forkBase Entry
	for i := benchTurns; i < benchTurns+8; i++ {
		if e := ask(fmt.Sprintf("follow up %d", i)); i == benchTurns+3 {
			forkBase = e
		}
		turn(i)
	}
	w.SetHead(forkBase.ID)
	ask("forked question")
	turn(benchTurns + 100)
	ask("one more")
	require.NoError(b, w.Sync())
	require.NoError(b, w.Close())
	return cd
}

// benchSession writes the fixture into a fresh store and returns its path.
func benchSession(b *testing.B) (*Store, string, string) {
	b.Helper()

	root := b.TempDir()
	store := StoreAt(root)
	workspace := filepath.Join(root, "ws")
	dir, err := store.Dir(workspace)
	require.NoError(b, err)
	path := filepath.Join(dir, "bench-session.jsonl")
	writeBenchTranscript(b, path)
	return store, workspace, path
}

// benchBranch resolves the transcript the way a resume does: Read, persisted
// head, Branch, then the compaction entry decoded from the branch.
func benchBranch(b *testing.B, path string) ([]Entry, CompactionData) {
	b.Helper()

	entries, warns, err := Read(path)
	require.NoError(b, err)
	require.Empty(b, warns)
	branch := Branch(entries, headFor(path, entries))
	require.NotEmpty(b, branch)

	var cd CompactionData
	for _, e := range branch {
		if e.Type == TypeCompaction {
			require.NoError(b, e.Decode(&cd))
		}
	}
	require.NotEmpty(b, cd.FirstKeptEntryID)
	return branch, cd
}

// BenchmarkResumeReplay measures the work a resume waits on. Sub-cases are
// cumulative: read alone, through Branch, then through ContextMessages with a
// model resolver, which is the full prompt rebuild.
func BenchmarkResumeReplay(b *testing.B) {
	_, _, path := benchSession(b)
	cd := func() CompactionData {
		_, data := benchBranch(b, path)
		return data
	}()
	resolve := func(string) (llm.Model, error) {
		return llm.Model{Provider: "anthropic", ID: "claude-opus-4-5", ContextWindow: 200000}, nil
	}

	b.Run("read", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := Read(path); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("read_branch", func(b *testing.B) {
		for b.Loop() {
			entries, _, err := Read(path)
			if err != nil {
				b.Fatal(err)
			}
			if branch := Branch(entries, headFor(path, entries)); len(branch) == 0 {
				b.Fatal("empty branch")
			}
		}
	})
	b.Run("read_branch_context", func(b *testing.B) {
		for b.Loop() {
			entries, _, err := Read(path)
			if err != nil {
				b.Fatal(err)
			}
			branch := Branch(entries, headFor(path, entries))
			msgs, warns := ContextMessages(branch, cd, resolve)
			if len(warns) > 0 {
				b.Fatalf("rebuild warnings: %v", warns)
			}
			if len(msgs) == 0 {
				b.Fatal("no messages rebuilt")
			}
		}
	})
}

// BenchmarkStoreScan measures the workspace scans behind the resume picker and
// prompt recall: List reads and branches every transcript, Prompts walks them
// for recorded user prompts.
func BenchmarkStoreScan(b *testing.B) {
	store, workspace, _ := benchSession(b)

	b.Run("list", func(b *testing.B) {
		for b.Loop() {
			infos, err := store.List(workspace)
			if err != nil {
				b.Fatal(err)
			}
			if len(infos) != 1 {
				b.Fatalf("got %d sessions, want 1", len(infos))
			}
		}
	})
	// a miss used to pay a second full directory scan for the damaged hint
	b.Run("find_miss", func(b *testing.B) {
		for b.Loop() {
			if _, err := store.Find(workspace, "no-such-target"); !errors.Is(err, ErrNotFound) {
				b.Fatalf("expected not-found, got %v", err)
			}
		}
	})
	b.Run("prompts", func(b *testing.B) {
		for b.Loop() {
			ps, err := store.Prompts(workspace)
			if err != nil {
				b.Fatal(err)
			}
			if len(ps) == 0 {
				b.Fatal("no prompts found")
			}
		}
	})
}

// benchWorkspace seeds n sibling transcripts so the scan benchmarks measure a
// directory, not one file.
func benchWorkspace(b *testing.B, n int) (*Store, string) {
	b.Helper()

	root := b.TempDir()
	store := StoreAt(root)
	workspace := filepath.Join(root, "ws")
	dir, err := store.Dir(workspace)
	require.NoError(b, err)
	for i := range n {
		writeBenchTranscript(b, filepath.Join(dir, fmt.Sprintf("bench-%03d.jsonl", i)))
	}
	return store, workspace
}

// BenchmarkStoreScanParallel measures List and Prompts over a many-session
// workspace, where the per-file parse pool pays for itself.
func BenchmarkStoreScanParallel(b *testing.B) {
	store, workspace := benchWorkspace(b, 12)

	b.Run("list", func(b *testing.B) {
		for b.Loop() {
			infos, err := store.List(workspace)
			if err != nil {
				b.Fatal(err)
			}
			if len(infos) != 12 {
				b.Fatalf("got %d sessions, want 12", len(infos))
			}
		}
	})
	b.Run("prompts", func(b *testing.B) {
		for b.Loop() {
			ps, err := store.Prompts(workspace)
			if err != nil {
				b.Fatal(err)
			}
			if len(ps) == 0 {
				b.Fatal("no prompts found")
			}
		}
	})
}

// BenchmarkResumeOpen measures what the startup snapshot saves: OpenEntries
// parses once for head recovery and state rebuild, while Open plus a separate
// Read (the pre-snapshot pattern) parses twice.
func BenchmarkResumeOpen(b *testing.B) {
	_, _, path := benchSession(b)

	b.Run("open_entries", func(b *testing.B) {
		for b.Loop() {
			w, entries, warns, err := OpenEntries(path)
			if err != nil || len(warns) > 0 {
				b.Fatal(err)
			}
			if len(entries) == 0 {
				b.Fatal("no entries")
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("open_plus_read", func(b *testing.B) {
		for b.Loop() {
			w, err := Open(path)
			if err != nil {
				b.Fatal(err)
			}
			entries, _, rerr := Read(path)
			if rerr != nil || len(entries) == 0 {
				b.Fatal("no entries")
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
