package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// helpMaxAnswer bounds one docs answer, so a question costs like the bounded
// call it is rather than an open-ended chat turn.
const helpMaxAnswer = 16384

// helpInstructions frames the doc block. It stays outside the provenance tags:
// the documents are data, these lines are the task.
const helpInstructions = `You answer questions about ajent, the coding agent whose session this is, using
only its own documentation below.

- Ground every claim in those documents and say plainly what they do not cover rather than guessing.
- Point at "ajent --help" or an in-session command such as /settings instead of inventing a flag, keybinding or config key.
- Answer in concise markdown.`

// helpAnswerHeader marks the printed block so a docs answer never reads like a turn.
const helpAnswerHeader = "*answered from ajent's own documentation (`/help`)*"

// helpDeps are the driver objects the /help controller closes over. The live
// session state is only ever read on the caller's goroutine (model), refused
// against (turns) and billed through (spend).
type helpDeps struct {
	ag          *agent.Agent // live agent; a turn streaming refuses an answer
	st          *agent.State // bills spend; Model is read before the answer goroutine starts
	ui          *hubUI       // notices, the markdown answer, the working glyph
	providerFor func(llm.Model) (llm.Provider, error)
	session     string    // request affinity, so repeat questions hit the cached doc block
	docs        []HelpDoc // README plus docs/features, embedded at build
}

// helpController answers /help questions from the embedded docs: one bare model
// call per question over a warm private thread. No tools, nothing appended to
// the live transcript or message list — restoring back to the last chat message
// is free because the session context was never left.
type helpController struct {
	deps   helpDeps
	system string // the doc block, built once so every request sends identical bytes

	mu     sync.Mutex
	cancel context.CancelFunc // in-flight answer, nil when idle
	// thread holds this run's Q&A pairs, touched only by the single live answer
	// goroutine; a next ask reads it after done() released the slot.
	thread []llm.Message
}

// newHelpController returns the controller with its system block built, or nil
// when the build carried no embedded docs. Both console seams then warn instead
// of arming or calling.
func newHelpController(deps helpDeps) *helpController {
	if len(deps.docs) == 0 || deps.ag == nil || deps.st == nil || deps.ui == nil || deps.providerFor == nil {
		return nil
	}
	docs := canonicalDocs(deps.docs)
	return &helpController{
		deps:   deps,
		system: helpSystem(docs),
	}
}

// canonicalDocs pins the doc block's order: README first, then the features
// files by name, whatever order the build handed them over.
func canonicalDocs(docs []HelpDoc) []HelpDoc {
	out := slices.Clone(docs)
	slices.SortFunc(out, func(a, b HelpDoc) int {
		ra, rb := 0, 0
		if a.Name != "README.md" {
			ra = 1
		}
		if b.Name != "README.md" {
			rb = 1
		}
		if c := ra - rb; c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// helpSystem builds the system block: instruction text outside one
// provenance-tagged copy of each document, concatenated in a fixed order.
func helpSystem(docs []HelpDoc) string {
	var b strings.Builder
	b.WriteString(helpInstructions)
	for _, d := range docs {
		tag := "ajent_features"
		if d.Name == "README.md" {
			tag = "ajent_readme"
		}
		b.WriteString("\n\n<" + tag + " path=\"" + d.Name + "\">\n")
		b.WriteString(strings.TrimRight(d.Body, "\n"))
		b.WriteString("\n</" + tag + ">")
	}
	return b.String()
}

// ask answers one question on its own goroutine, so the handler returns at once
// and a slow provider never stalls the pump. Refused while a turn streams or an
// answer is already up; neither case queues.
func (c *helpController) ask(ctx context.Context, question string) {
	if c.deps.ag.Running() {
		c.deps.ui.Notify("help refused: press Esc to stop the turn first", tui.LevelWarn)
		return
	}
	// snapshot here on the caller's goroutine: SetModel writes st.Model on the
	// pump, and a read from the answer goroutine would race it
	model := c.deps.st.Model
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		cancel()
		c.deps.ui.Notify("help refused: an answer is already running", tui.LevelWarn)
		return
	}
	c.cancel = cancel
	c.mu.Unlock()

	go func() {
		defer c.done()
		c.answer(ctx, model, question)
	}()
}

// abort ends an in-flight answer and reports whether there was one. The stop
// surfaces as the answer's own cancelled notice.
func (c *helpController) abort() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// done releases the in-flight slot; every path out of answer cancels its context.
func (c *helpController) done() {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil { // abort may have fired it already, a repeat is a no-op
		cancel()
	}
}

// answer runs the request behind one question: docs as the system block, the
// question verbatim as its own user turn. model is ask's caller-goroutine
// snapshot; Spend bills the live ledger without touching context occupancy.
// Success prints through Print beside the echoed question.
func (c *helpController) answer(ctx context.Context, model llm.Model, question string) {
	if model.ID == "" {
		c.deps.ui.Notify("help unavailable: no model configured", tui.LevelWarn)
		return
	}
	provider, err := c.deps.providerFor(model)
	if err != nil {
		c.deps.ui.Notify(fmt.Sprintf("help unavailable: no provider for %s (%v)", model.Key(), err), tui.LevelWarn)
		return
	}
	done := c.deps.ui.Busy()
	defer done()

	budget := helpMaxAnswer
	if model.MaxOutput > 0 {
		budget = min(budget, model.MaxOutput)
	}
	req := llm.Request{
		Model:     model,
		System:    llm.BlockList{llm.TextBlock{Text: c.system}},
		Messages:  append(slices.Clone(c.thread), userQuestion(question)),
		MaxTokens: budget,
		Cache:     llm.CachePolicy{Enabled: true},
		SessionID: c.deps.session, // one doc block per run, cacheable across questions
	}
	req.Reasoning = llm.ReasoningConfig{Level: llm.ClampLevel(model, llm.LevelOff)}

	text, usage, err := llm.RunSummary(ctx, provider, req)
	// bill-only, as compaction does: a failed call still spent what arrived
	c.deps.st.Tokens.Spend(model.Key(), usage)
	switch {
	case errors.Is(err, context.Canceled):
		c.deps.ui.Notify("help cancelled", tui.LevelWarn)
	case errors.Is(err, llm.ErrTruncated):
		// RunSummary discards partial text on a cap stop, so the ask has to be retried narrower
		c.deps.ui.Notify("help answer hit its length cap; try a narrower question", tui.LevelWarn)
	case err != nil:
		c.deps.ui.Notify("help failed: "+err.Error(), tui.LevelWarn)
	default:
		c.thread = append(c.thread, userQuestion(question),
			llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{llm.TextBlock{Text: text}}})
		c.deps.ui.Print(helpAnswerHeader + "\n\n" + text)
	}
}

// userQuestion wraps one question as its request message.
func userQuestion(q string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: q}}}
}
