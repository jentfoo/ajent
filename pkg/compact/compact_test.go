package compact

import (
	"context"
	"encoding/json"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// builders ------------------------------------------------------------

func msg(id string, m llm.Message) session.Entry {
	b, _ := json.Marshal(session.MessageData{Message: m})
	return session.Entry{ID: id, Type: session.TypeMessage, Data: b}
}

func userText(id, text string) session.Entry { return msg(id, llm.Text(llm.RoleUser, text)) }

func assistText(id, text string) session.Entry { return msg(id, llm.Text(llm.RoleAssistant, text)) }

func callMsg(id, callID, name, path string) session.Entry {
	input := json.RawMessage(`{}`)
	if path != "" {
		input = json.RawMessage(`{"path":"` + path + `"}`)
	}
	m := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
		llm.ToolCallBlock{ID: callID, Name: name, Input: input},
	}}
	return msg(id, m)
}

func resultMsg(id, callID, text string, isError bool) session.Entry {
	m := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.ToolResultBlock{CallID: callID, IsError: isError,
			Content: llm.BlockList{llm.TextBlock{Text: text}}},
	}}
	return msg(id, m)
}

func compactEntry(id, summary, firstKept string) session.Entry {
	b, _ := json.Marshal(session.CompactionData{Summary: summary, FirstKeptEntryID: firstKept})
	return session.Entry{ID: id, Type: session.TypeCompaction, Data: b}
}

// stage 1 ------------------------------------------------------------

func TestStructuralFailedOldStubbed(t *testing.T) {
	t.Parallel()

	// a failed result four turns back is older than the last two turns.
	branch := []session.Entry{
		userText("u1", "q1"), // turn 1
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", "boom: no such file\nmore detail", true),
		userText("u2", "q2"), // turn 2
		userText("u3", "q3"), // turn 3
		userText("u4", "q4"), // turn 4
	}
	stubs, drops, stats := structural(branch, "/tmp")
	require.Len(t, stubs, 1)
	assert.Equal(t, "c1", stubs[0].CallID)
	assert.Contains(t, stubs[0].Text, "bash failed")
	assert.Contains(t, stubs[0].Text, "boom: no such file") // first line preserved
	assert.Empty(t, drops)
	assert.Equal(t, 1, stats.Failed)
}

func TestStructuralFailedRecentKept(t *testing.T) {
	t.Parallel()

	// a failure in the current turn is still live context, not dead weight.
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", "boom", true),
	}
	stubs, _, stats := structural(branch, "/tmp")
	assert.Empty(t, stubs)
	assert.Zero(t, stats.Failed)
}

func TestStructuralSupersededRead(t *testing.T) {
	t.Parallel()

	// reading the same path twice keeps the newest and stubs the first.
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "read", "main.go"),
		resultMsg("r1", "c1", "old contents", false),
		userText("u2", "q2"),
		callMsg("a2", "c2", "read", "main.go"),
		resultMsg("r2", "c2", "new contents", false),
	}
	stubs, _, stats := structural(branch, "/tmp")
	require.Len(t, stubs, 1)
	assert.Equal(t, "c1", stubs[0].CallID) // the older read is stubbed
	assert.Contains(t, stubs[0].Text, "superseded")
	assert.Equal(t, 1, stats.Superseded)
}

func TestStructuralEditBeforeWrite(t *testing.T) {
	t.Parallel()

	// an edit whose file is later rewritten wholesale is stubbed.
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "edit", "main.go"),
		resultMsg("r1", "c1", "applied diff", false),
		userText("u2", "q2"),
		callMsg("a2", "c2", "write", "main.go"),
		resultMsg("r2", "c2", "wrote file", false),
	}
	stubs, _, stats := structural(branch, "/tmp")
	require.Len(t, stubs, 1)
	assert.Equal(t, "c1", stubs[0].CallID)
	assert.Equal(t, 1, stats.Superseded)
}

func TestStructuralEditAfterWriteKept(t *testing.T) {
	t.Parallel()

	// an edit that comes AFTER a write to the same file is legitimate (it builds on
	// the rewrite), so only edits preceding a later write are stubbed.
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "write", "main.go"),
		resultMsg("r1", "c1", "wrote file", false),
		userText("u2", "q2"),
		callMsg("a2", "c2", "edit", "main.go"), // edit after the write: kept
		resultMsg("r2", "c2", "applied diff on top", false),
	}
	stubs, _, stats := structural(branch, "/tmp")
	assert.Empty(t, stubs) // no later write supersedes this edit
	assert.Zero(t, stats.Superseded)
}

func TestStructuralEditBetweenWritesStubbedOnce(t *testing.T) {
	t.Parallel()

	// an intermediate edit between two writes is still superseded by the last one.
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "edit", "main.go"),
		resultMsg("r1", "c1", "first diff", false),
		userText("u2", "q2"),
		callMsg("a2", "c2", "write", "main.go"),
		resultMsg("r2", "c2", "wrote v1", false),
		userText("u3", "q3"),
		callMsg("a3", "c3", "edit", "main.go"), // before the final write: stubbed
		resultMsg("r3", "c3", "second diff", false),
		userText("u4", "q4"),
		callMsg("a4", "c4", "write", "main.go"), // the last write rewrites everything
		resultMsg("r4", "c4", "wrote v2", false),
	}
	stubs, _, stats := structural(branch, "/tmp")
	// both intermediate edits are superseded by a later wholesale write; the final
	// write itself is not an edit and so is never stubbed.
	require.Len(t, stubs, 2)
	assert.Equal(t, "c1", stubs[0].CallID)
	assert.Equal(t, "c3", stubs[1].CallID)
	assert.Equal(t, 2, stats.Superseded)
}

func TestStructuralAbortedDropped(t *testing.T) {
	t.Parallel()

	// an assistant message with no tool calls and no text is dropped by entry id.
	branch := []session.Entry{
		userText("u1", "q1"),
		assistText("a1", ""), // aborted
		userText("u2", "q2"),
	}
	_, drops, stats := structural(branch, "/tmp")
	assert.Equal(t, []string{"a1"}, drops)
	assert.Equal(t, 1, stats.Aborted)
}

func TestStructuralAssistantWithToolCallNotDropped(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""), // carries a tool call, never dropped
		resultMsg("r1", "c1", "ok", false),
	}
	_, drops, _ := structural(branch, "/tmp")
	assert.Empty(t, drops)
}

// stage 2 ------------------------------------------------------------

func TestTruncateElidesOldLargeOutput(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 16<<10)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", big, false),
		userText("u2", "q2"),
		userText("u3", "q3"),
		userText("u4", "q4"),
		userText("u5", "q5"),
	}
	r := &session.Reduce{}
	out := truncate(branch, "/tmp", r)
	require.Len(t, out, 1)
	assert.Equal(t, "c1", out[0].CallID)
	assert.Positive(t, out[0].Limit)
	assert.Less(t, out[0].Limit, len(big)) // elided below the original
}

func TestTruncateKeepsCurrentTurn(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 16<<10)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", big, false), // current turn: left alone
	}
	r := &session.Reduce{}
	out := truncate(branch, "/tmp", r)
	assert.Empty(t, out)
}

func TestTruncateDuplicateOutputs(t *testing.T) {
	t.Parallel()

	same := strings.Repeat("line\n", 300)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", same, false),
		userText("u2", "q2"),
		callMsg("a2", "c2", "bash", ""),
		resultMsg("r2", "c2", same, false), // identical: stubbed
	}
	r := &session.Reduce{}
	out := truncate(branch, "/tmp", r)
	require.Len(t, out, 1)
	assert.Equal(t, "c2", out[0].CallID)
	assert.Contains(t, out[0].Text, "duplicate")
}

func TestTruncateDuplicateOutputsDifferentTargetsKept(t *testing.T) {
	t.Parallel()

	// two results with identical bytes from different files are distinct context, so
	// they must not be collapsed as duplicates.
	same := strings.Repeat("line\n", 300)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "read", "one.txt"),
		resultMsg("r1", "c1", same, false),
		userText("u2", "q2"),
		callMsg("a2", "c2", "read", "two.txt"), // different file
		resultMsg("r2", "c2", same, false),     // identical bytes, but not a duplicate
	}
	r := &session.Reduce{}
	out := truncate(branch, "/tmp", r)
	assert.Empty(t, out) // neither is collapsed; both are distinct targets
}

// cut point ------------------------------------------------------------

func TestSelectCutPrefersTurnStart(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", strings.Repeat("a ", 400)),
		assistText("a1", strings.Repeat("b ", 400)),
		userText("u2", strings.Repeat("c ", 400)),
		assistText("a2", strings.Repeat("d ", 400)),
	}
	cut := selectCut(branch, 500) // keep roughly the last turn
	require.NotEqual(t, -1, cut)
	// the cut must land on a turn start or an assistant, never a tool result.
	assert.True(t, isValidCut(branch[cut]))
}

func TestSelectCutWholeBranchFits(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{userText("u1", "hi"), assistText("a1", "yo")}
	assert.Equal(t, -1, selectCut(branch, 100000)) // nothing to cut
}

func TestSelectCutKeepsTailWellFormed(t *testing.T) {
	t.Parallel()

	// a tool call followed by its result: any cut must keep them paired.
	branch := []session.Entry{
		userText("u1", strings.Repeat("q ", 300)),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", strings.Repeat("o ", 300), false),
		userText("u2", strings.Repeat("r ", 300)),
		assistText("a2", strings.Repeat("s ", 300)),
	}
	for budget := 50; budget <= 2000; budget += 50 {
		cut := selectCut(branch, budget)
		if cut == -1 {
			continue
		}
		assert.True(t, isValidCut(branch[cut]), "budget %d cut at a non-boundary", budget)
		assert.True(t, tailWellFormed(branch, cut), "budget %d orphans a tool call", budget)
	}
}

// tailWellFormed reports whether every tool call in [cut:] is answered within it.
func tailWellFormed(branch []session.Entry, cut int) bool {
	calls := 0
	for i := cut; i < len(branch); i++ {
		if branch[i].Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		if err := branch[i].Decode(&md); err != nil {
			continue
		}
		for _, b := range md.Message.Content {
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

// summarisation ------------------------------------------------------------

func TestCompactSummariserPromptCarriesInstructions(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "refactor the auth package"),
		assistText("a1", strings.Repeat("working on auth ", 200)),
		userText("u2", "now update the tests"),
		assistText("a2", strings.Repeat("tests updated ", 80)),
	}
	var got llm.Request
	run := func(_ context.Context, req llm.Request) (string, error) {
		got = req
		return "## Goal\nrefactor auth", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 600, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{
		Instructions: "focus on the auth refactor; keep the file paths",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "## Goal\nrefactor auth", res.Summary)
	assert.NotEmpty(t, res.FirstKeptEntryID)
	assert.Less(t, res.After, res.Before)

	require.Len(t, got.Messages, 1)
	prompt := textOf(got.Messages[0])
	assert.Contains(t, prompt, "<conversation>")
	assert.Contains(t, prompt, "## Goal")  // the six-section spec
	assert.Contains(t, prompt, "synopsis") // produced-content detail rule
	assert.Contains(t, prompt, "Additional focus: focus on the auth refactor")
}

// A manual /compact must fold older turns into a summary even when the whole
// session sits far below half the window, so it never reports "nothing to compact"
// on an ordinary short conversation. It keeps only the most recent user turn.
func TestCompactForceSummarisesSmallSession(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", strings.Repeat("first ask ", 40)),
		assistText("a1", strings.Repeat("first answer ", 60)),
		userText("u2", "second question"),
		assistText("a2", strings.Repeat("current reply ", 10)),
	}
	called := false
	run := func(_ context.Context, _ llm.Request) (string, error) {
		called = true
		return "## Goal\nrefactor auth", nil
	}
	// a huge window: without Force the whole branch fits and nothing would happen.
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{Force: true})
	require.NoError(t, err)
	require.NotNil(t, res) // must reduce, not report "nothing to compact"
	assert.True(t, called)
	assert.Equal(t, "## Goal\nrefactor auth", res.Summary)

	// the kept tail starts at u2 (the most recent real user prompt): only the final
	// exchange survives verbatim; everything before it is summarised away.
	require.NotEmpty(t, res.FirstKeptEntryID)
	tail := []string{}
	keep := false
	for _, e := range branch {
		if e.ID == res.FirstKeptEntryID {
			keep = true
		}
		if keep && e.Type == session.TypeMessage {
			tail = append(tail, e.ID)
		}
	}
	assert.Equal(t, []string{"u2", "a2"}, tail)
	// the summary plus one exchange must cost less than all four messages.
	cd := session.CompactionData{Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID}
	after := tokens.EstimateMessages(mustMsgs(t, branch, cd))
	before := tokens.EstimateMessages(mustMsgs(t, branch, session.CompactionData{}))
	assert.Less(t, after, before)
}

// mustMsgs returns the messages ContextMessages would send for branch under cd.
func mustMsgs(t *testing.T, branch []session.Entry, cd session.CompactionData) []llm.Message {
	t.Helper()
	msgs, warns := session.ContextMessages(branch, cd, nil)
	require.Empty(t, warns)
	return msgs
}

// A forced compact on a single exchange has no older turn to keep: the whole
// history folds into a summary-only plan.
func TestCompactForceSingleTurnSummaryOnly(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "Read me a short sci-fi story"),
		assistText("a1", strings.Repeat("Mira ran her hand along the spines. ", 100)),
	}
	run := func(_ context.Context, _ llm.Request) (string, error) {
		return "## Goal\nwanted a story\n## Progress\n### Done\n- [x] told The Last Librarian", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{Force: true})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Empty(t, res.FirstKeptEntryID) // nothing survives verbatim
	assert.NotEmpty(t, res.Summary)
	assert.Less(t, res.After, res.Before)

	// once recorded, the rebuilt context is the summary plus anything newer
	recorded := append(slices.Clone(branch), compactEntry("c1", res.Summary, ""), userText("u2", "another?"))
	msgs, warns := session.ContextMessages(recorded, session.CompactionData{Summary: res.Summary}, nil)
	require.Empty(t, warns)
	require.Len(t, msgs, 2)
	assert.Contains(t, textOf(msgs[0]), "The Last Librarian")
	assert.Equal(t, "another?", textOf(msgs[1]))
}

// A forced compact whose summary cannot shrink the conversation is refused, never
// recorded as a growth.
func TestCompactForceRefusesWhenSummaryWouldGrow(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{userText("u1", "hi"), assistText("a1", "yo")}
	run := func(_ context.Context, _ llm.Request) (string, error) {
		return "## Goal\n" + strings.Repeat("wordy ", 50), nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{Force: true})
	require.NoError(t, err)
	assert.Nil(t, res)
}

// A blank summariser response is a failure, never a silent "nothing to compact".
func TestCompactEmptySummaryIsAnError(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "Read me a short sci-fi story"),
		assistText("a1", strings.Repeat("Mira ran her hand along the spines. ", 100)),
	}
	run := func(_ context.Context, _ llm.Request) (string, error) { return "  \n", nil }
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{Force: true})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "empty summary")
}

// The summariser budget must stay below the span it replaces so even a generous
// summary still shrinks the context.
func TestSummarizeBudgetBoundedBySpan(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000} // reserve 40k
	assert.Equal(t, 300, summarizeBudget(model, 600))                    // small span: half of it
	assert.Equal(t, 32000, summarizeBudget(model, 1000000))              // huge span: 80% of reserve
	assert.Equal(t, 256, summarizeBudget(model, 8))                      // floor for a trivial span
}

// Regression: a cut that keeps nearly everything and adds a summary on top grows
// the context; it must never be recorded as a compaction ("compacted 500 -> 688").
func TestCompactNeverRecordsGrowth(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "Read me a short sci-fi story"),
		assistText("a1", strings.Repeat("Mira ran her hand along the spines. ", 100)),
	}
	run := func(_ context.Context, _ llm.Request) (string, error) {
		return "## Goal\nplaceholder summary body that is not tiny", nil
	}
	// a small window so the automatic path reaches stage 4; the one large reply
	// alone exceeds the target, so the natural cut keeps it and gains nothing.
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 900, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{})
	require.NoError(t, err)
	assert.Nil(t, res)
}

// A summary-only compaction bounds the next run's span: only messages recorded
// after it are new, so the folded region is never re-summarised or resurrected.
func TestPriorSpanStartAfterSummaryOnly(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "old ask"),
		assistText("a1", "old answer"),
		compactEntry("comp", "everything folded", ""),
		userText("u2", "new ask"),
	}
	start, ok := priorSpanStart(branch)
	assert.True(t, ok)
	assert.Equal(t, 3, start)
}

func TestCompactSummariserMergesPreviousSummary(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "first ask"),
		compactEntry("comp", "an earlier summary", "u2"),
		userText("u2", "second ask"),
		assistText("a2", strings.Repeat("more work ", 200)),
		userText("u3", "third ask"),
		assistText("a3", strings.Repeat("tail reply ", 120)),
	}
	var got llm.Request
	run := func(_ context.Context, req llm.Request) (string, error) {
		got = req
		return "merged", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 600, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Less(t, res.After, res.Before)

	prompt := textOf(got.Messages[0])
	assert.Contains(t, prompt, "<previous-summary>")
	assert.Contains(t, prompt, "an earlier summary")
	assert.Contains(t, prompt, "PRESERVE all existing information") // incremental rules
}

func TestCompactStagesMeetTargetNoSummary(t *testing.T) {
	t.Parallel()

	// a huge failed result far from the current turn is stubbed by stage 1 alone;
	// with a generous target no summariser call is needed.
	big := strings.Repeat("e", 64<<10)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", big, true),
		userText("u2", "q2"),
		userText("u3", "q3"),
		userText("u4", "q4"),
	}
	var called bool
	run := func(_ context.Context, _ llm.Request) (string, error) {
		called = true
		return "", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(context.Background(), branch, model, run, Options{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, called, "stages 1-3 met the target without a summary")
	assert.Empty(t, res.Summary)
	assert.Less(t, res.After, res.Before)
	assert.Equal(t, 1, res.Reduce.Stats.Failed)
}

func TestCompactNoChangeReturnsNil(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{userText("u1", "hi"), assistText("a1", "yo")}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}
	res, err := Compact(context.Background(), branch, model, nil, Options{})
	require.NoError(t, err)
	assert.Nil(t, res) // nothing to reduce, no summary wired
}

// textOf returns the first text block of a message.
func textOf(m llm.Message) string {
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// TestSelectCutFuzzWellFormed generates branches with random tool interleavings
// and asserts every cut keeps a well-formed tail: no orphaned tool_use, and the
// cut never lands on a tool-result-only message.
func TestSelectCutFuzzWellFormed(t *testing.T) {
	t.Parallel()

	for seed := int64(0); seed < 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		branch := randomBranch(r)
		for budget := 30; budget <= 3000; budget += 97 {
			cut := selectCut(branch, budget)
			if cut == -1 {
				continue
			}
			assert.True(t, isValidCut(branch[cut]),
				"seed %d budget %d cut at a non-boundary", seed, budget)
			assert.True(t, tailWellFormed(branch, cut),
				"seed %d budget %d orphans a tool call", seed, budget)
		}
	}
}

// randomBranch builds a plausible branch mixing user prompts, assistant text and
// tool call/result pairs in random order.
func randomBranch(r *rand.Rand) []session.Entry {
	var branch []session.Entry
	id := 0
	next := func() string { id++; return "e" + strconv.Itoa(id) }
	turns := 2 + r.Intn(5)
	for tn := 0; tn < turns; tn++ {
		branch = append(branch, userText(next(), strings.Repeat("q ", 20+r.Intn(80))))
		steps := r.Intn(3)
		for s := 0; s < steps; s++ {
			cid := "c" + strconv.Itoa(id) + "_" + strconv.Itoa(s)
			branch = append(branch, callMsg(next(), cid, "bash", ""))
			branch = append(branch, resultMsg(next(), cid, strings.Repeat("o ", 10+r.Intn(120)), r.Intn(4) == 0))
		}
		if r.Intn(2) == 0 {
			branch = append(branch, assistText(next(), strings.Repeat("a ", 10+r.Intn(60))))
		}
	}
	return branch
}

func TestCompactNoDuplicateStubs(t *testing.T) {
	t.Parallel()

	// a failed result (stage 1) and a large old result (stage 2) must each be
	// stubbed exactly once, never twice.
	big := strings.Repeat("y", 16<<10)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", "failed output", true),
		callMsg("a2", "c2", "bash", ""),
		resultMsg("r2", "c2", big, false),
		userText("u2", "q2"),
		userText("u3", "q3"),
		userText("u4", "q4"),
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}
	res, err := Compact(context.Background(), branch, model, nil, Options{})
	require.NoError(t, err)
	require.NotNil(t, res)

	seen := make(map[string]int)
	for _, s := range res.Reduce.Stubs {
		seen[s.CallID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "call id %s stubbed more than once", id)
	}
}
