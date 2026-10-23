package llm

import (
	"encoding/json"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
)

// maxCompatCallID is openai's cap on a chat-completions tool_call_id.
const maxCompatCallID = 40

// imageOmitted and toolImageOmitted are the placeholders a text-only model
// receives in place of an unsupported image block.
const (
	imageOmitted     = "(image omitted: model does not support images)"
	toolImageOmitted = "(tool image omitted: model does not support images)"
)

// noResult is the synthetic content answering an orphaned tool call.
const noResult = "No result provided"

// processedTools is the bridging assistant reply some chat-completions providers
// require between a tool-result turn and the next user message.
const processedTools = "I have processed the tool results."

// Prepare returns req with its messages normalized for the target model: images
// downgraded, leaked inline reasoning split back into thinking blocks, foreign
// reasoning degraded to text or dropped, retention applied, tool-call ids made
// legal and reused ones disambiguated, unanswered calls answered, malformed
// tool-call inputs replaced with {}. Every request path passes through it so the
// estimator and the wire never disagree about what is sent. It is idempotent, so
// calling it twice yields an identical message list.
//
// The pass never writes through req.Messages: clean stages share the input's
// backing arrays untouched and the origin stamp clones on first write.
func Prepare(req Request) Request {
	caps := req.Model.Caps
	target := Origin{Provider: req.Model.Provider, Dialect: caps.Dialect, Model: req.Model.ID}

	msgs := downgradeImages(req.Messages, caps)
	msgs = splitInlineThinking(msgs, caps)
	msgs = normalizeContent(msgs, req.Reasoning.Retain, caps, target)
	msgs = splitToolResultImages(msgs, caps) // B4: compat carries tool images separately
	msgs = repairToolInputs(msgs)            // invalid args ride as {}, never raw
	msgs = repairTurns(msgs, caps)

	// stamp the resolved origin so a later Prepare pass sees same-model messages.
	// Writes clone first: msgs may share the caller's backing array by now, and an
	// in-place stale origin would reclassify id normalization on the next switch
	var stamped []Message // cloned messages once a stamp lands; nil while none do
	for i := range msgs {
		m := msgs[i]
		if m.Origin != nil && *m.Origin == target {
			continue
		}
		if stamped == nil {
			stamped = slices.Clone(msgs)
		}
		m.Origin = &target
		stamped[i] = m
	}
	if stamped != nil {
		msgs = stamped
	}
	req.Messages = msgs
	return req
}

// takeBlocks returns the list a rewrite of src at slot j appends into: an
// existing rebuild passes through, otherwise everything before j is cloned so
// later writes never land in shared memory.
func takeBlocks(content, src BlockList, j int) BlockList {
	if content != nil {
		return content
	}
	out := make(BlockList, 0, len(src))
	return append(out, src[:j]...)
}

// keepBlock appends b once a rebuild is underway; before that the input slice
// already holds it verbatim, so an unchanged message keeps sharing its array.
func keepBlock(content BlockList, b Block) BlockList {
	if content == nil {
		return nil
	}
	return append(content, b)
}

// splitInlineThinking moves reasoning that leaked into an assistant text block
// back out into thinking blocks for chat-completions models emitting tags on
// the content channel. One stray tag makes template-rendering servers re-emit
// the message every later turn, multiplying the prompt past what the estimator
// counted. The transcript stays verbatim, only the request copy is restructured.
func splitInlineThinking(msgs []Message, caps Capabilities) []Message {
	if caps.Dialect != DialectOpenAICompletions || !caps.Reasoning {
		return msgs
	}
	open, close := thinkTagsFor(caps)
	var out []Message
	for i, m := range msgs {
		if m.Role != RoleAssistant || !hasThinkMarker(m.Content, open, close) {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		// the channel this turn's reasoning arrived on is the channel the recovered
		// tail belongs to: a surviving sibling block carries its delta field, and the
		// configured field is the fallback when the whole turn leaked
		field := caps.ReasoningField
		for _, b := range m.Content {
			if tb, ok := b.(ThinkingBlock); ok && tb.Field != "" {
				field = tb.Field
			}
		}
		content := make(BlockList, 0, len(m.Content)+2)
		for _, b := range m.Content {
			if tb, ok := b.(TextBlock); ok {
				content = append(content, splitThinkText(tb.Text, open, close, field)...)
				continue
			}
			content = append(content, b)
		}
		m.Content = content
		out[i] = m
	}
	if out == nil {
		return msgs
	}
	return out
}

// thinkTagsFor resolves the inline reasoning markers for a model, falling back
// to the conventional think tags when the capability carries none.
func thinkTagsFor(caps Capabilities) (open, close string) {
	if caps.ThinkOpen != "" && caps.ThinkClose != "" {
		return caps.ThinkOpen, caps.ThinkClose
	}
	return thinkOpenTag, thinkCloseTag
}

// hasThinkMarker reports whether any text block carries an inline marker.
func hasThinkMarker(content BlockList, open, close string) bool {
	return slices.ContainsFunc(content, func(b Block) bool {
		tb, ok := b.(TextBlock)
		return ok && (strings.Contains(tb.Text, open) || strings.Contains(tb.Text, close))
	})
}

// splitThinkText splits one text at its reasoning markers, returning the
// regions as thinking and text blocks in order. A close tag with no opening tag
// opens the region at the previous boundary, which is how a reasoning tail that
// leaked into the text channel reads: the opening tag sits further back, before
// reasoning already recorded as its own block. field rides the recovered block
// so providers that replay thinking send it back on the right channel.
func splitThinkText(text, open, close, field string) BlockList {
	type region struct {
		thinking bool
		text     string
	}
	var regions []region
	var inside bool
	for text != "" {
		tag, at := nextThinkTag(text, open, close)
		if at < 0 {
			regions = append(regions, region{inside, text})
			break
		}
		if head := text[:at]; head != "" {
			// a close with no open opens the region implicitly
			regions = append(regions, region{inside || tag == close, head})
		}
		inside = tag == open
		text = text[at+len(tag):]
	}

	out := make(BlockList, 0, len(regions))
	for _, r := range regions {
		if r.thinking {
			out = append(out, ThinkingBlock{Text: r.text, Field: field})
		} else {
			out = append(out, TextBlock{Text: r.text})
		}
	}
	return out
}

// nextThinkTag finds whichever marker occurs first, returning it and its offset.
// Neither present reports the open tag at -1.
func nextThinkTag(text, open, close string) (string, int) {
	tag, at := open, strings.Index(text, open)
	if j := strings.Index(text, close); j >= 0 && (at < 0 || j < at) {
		tag, at = close, j
	}
	return tag, at
}

// downgradeImages replaces image blocks with a placeholder when the model does
// not accept them, collapsing consecutive placeholders to one. Every role gets
// the same placeholder, so a resumed assistant image never fails the request.
func downgradeImages(msgs []Message, caps Capabilities) []Message {
	if caps.Images || !hasImageBlock(msgs) {
		return msgs
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := make(BlockList, 0, len(m.Content))
		for _, b := range m.Content {
			switch v := b.(type) {
			case ImageBlock:
				content = appendPlaceholder(content, imageOmitted)
			case ToolResultBlock:
				if hasImage(v.Content) {
					v.Content = downgradeBlocks(v.Content, toolImageOmitted)
				}
				content = append(content, v)
			default:
				content = append(content, b)
			}
		}
		m.Content = content
		out = append(out, m)
	}
	return out
}

func hasImageBlock(msgs []Message) bool {
	return slices.ContainsFunc(msgs, func(m Message) bool { return slices.ContainsFunc(m.Content, blockHasImage) })
}

// blockHasImage reports whether a block is an image or carries one in its content.
func blockHasImage(b Block) bool {
	switch v := b.(type) {
	case ImageBlock:
		return true
	case ToolResultBlock:
		return hasImage(v.Content)
	default:
		return false
	}
}

// downgradeBlocks maps each image to a placeholder text block, collapsing runs.
func downgradeBlocks(blocks BlockList, placeholder string) BlockList {
	out := make(BlockList, 0, len(blocks))
	for _, b := range blocks {
		if _, ok := b.(ImageBlock); ok {
			out = appendPlaceholder(out, placeholder)
			continue
		}
		out = append(out, b)
	}
	return out
}

// appendPlaceholder adds p unless the previously emitted block was already it.
func appendPlaceholder(blocks BlockList, p string) BlockList {
	if n := len(blocks); n > 0 {
		if t, ok := blocks[n-1].(TextBlock); ok && t.Text == p {
			return blocks
		}
	}
	return append(blocks, TextBlock{Text: p})
}

func hasImage(content BlockList) bool {
	return slices.ContainsFunc(content, func(b Block) bool { return b.blockType() == BlockImage })
}

// splitToolResultImages moves images out of compat tool results into a following
// user message, since chat-completions cannot carry them inside the tool role. It
// gates on usesCompatBody so a responses model riding the compat fallback splits
// too. It runs after the placeholder ladder so each result keeps its "(see attached
// image)" text, and is idempotent: the second pass finds no images left in any result.
func splitToolResultImages(msgs []Message, caps Capabilities) []Message {
	if !usesCompatBody(caps) || !caps.Images || !hasImageBlock(msgs) {
		return msgs // no image anywhere: the common path skips the per-message clones
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := slices.Clone(m.Content)
		var images BlockList // image blocks pulled from tool results this message
		for i, b := range content {
			tr, ok := b.(ToolResultBlock)
			if !ok || !hasImage(tr.Content) {
				continue
			}
			var kept BlockList
			for _, cb := range tr.Content {
				if img, isImg := cb.(ImageBlock); isImg {
					images = append(images, img)
				} else {
					kept = append(kept, cb)
				}
			}
			tr.Content = kept
			content[i] = tr
		}
		m.Content = content
		out = append(out, m)
		if len(images) > 0 {
			// a user message carries the images with the tool-result lead-in text.
			// RequiresAssistantAfterToolResult then places its bridge ahead of it in repairTurns.
			attach := make([]Block, 1, len(images)+1)
			attach[0] = TextBlock{Text: "Attached image(s) from tool result:"}
			out = append(out, Message{Role: RoleUser, Content: BlockList(append(attach, images...))})
		}
	}
	return out
}

// normalizeContent merges retention with cross-model degradation: thinking kept,
// degraded to text or dropped per the resolved policy against target origin,
// foreign text signatures stripped, tool-call ids normalized for the target.
//
// Call ids rewrite consistently across one request, keeping every result
// attached to its call. Assistant messages emptied by the pass are dropped. A
// list needing none of that passes through untouched.
func normalizeContent(msgs []Message, policy RetainPolicy, caps Capabilities, target Origin) []Message {
	policy = resolveRetention(policy, caps)
	keepFrom, lastAssistant := retentionBounds(msgs)

	var out []Message // nil while every message passes through untouched
	// fork copies the messages already passed through on the first change, so a
	// clean pass returns the caller's slice and a dirty one owns its output
	fork := func(i int) {
		if out == nil {
			out = slices.Clone(msgs[:i])
		}
	}
	rename := map[string]string{} // original foreign call id -> normalized

	for i, m := range msgs {
		if m.Role != RoleAssistant || !hasThinking(m.Content) {
			nm, changed := degradeForeignBlocks(m, caps, target, rename)
			if changed {
				fork(i)
			}
			if out != nil {
				out = append(out, nm)
			}
			continue
		}

		keep := keepThinking(policy, i, keepFrom, lastAssistant)
		foreign := originForeign(m.Origin, target) // full mismatch: any model switch
		kind := callKindFor(m.Origin, target)      // endpoint vs same-endpoint for ids

		var content BlockList // rebuilt blocks once a change lands in this message
		for j, b := range m.Content {
			switch v := b.(type) {
			case ThinkingBlock:
				if !keep || foreign && (v.Redacted != "" || strings.TrimSpace(v.Text) == "") {
					content = takeBlocks(content, m.Content, j)
					continue // dropped: policy, or an un-replayable foreign token
				}
				if foreign {
					// a foreign reasoning block degrades to visible text so the turn survives
					content = takeBlocks(content, m.Content, j)
					content = append(content, TextBlock{Text: v.Text})
					continue
				}
				switch {
				case caps.RequiresThinkingAsText:
					if strings.TrimSpace(v.Text) != "" {
						content = takeBlocks(content, m.Content, j)
						content = append(content, TextBlock{Text: v.Text})
					} else if replayable(v, caps) && (v.Signature != "" || v.Redacted != "") {
						content = keepBlock(content, v)
					} else {
						content = takeBlocks(content, m.Content, j) // nothing to send back
					}
				case !replayable(v, caps):
					content = takeBlocks(content, m.Content, j) // dropped: no token this provider accepts
				default:
					if strings.TrimSpace(v.Text) == "" && v.Signature == "" && v.Redacted == "" &&
						v.ItemID == "" && len(v.Item) == 0 && len(v.Details) == 0 {
						content = takeBlocks(content, m.Content, j)
						continue // nothing to replay at all
					}
					content = keepBlock(content, v)
				}
			case TextBlock:
				if foreign && v.Signature != "" {
					v.Signature = ""
					content = takeBlocks(content, m.Content, j)
					content = append(content, v)
					continue
				}
				content = keepBlock(content, v)
			case ToolCallBlock:
				if kind != callSameEndpoint {
					nid := normalizeCallID(v.ID, caps, target.Provider, kind)
					if nid != v.ID {
						rename[v.ID] = nid
						v.ID = nid
						content = takeBlocks(content, m.Content, j)
						content = append(content, v)
						continue
					}
				}
				content = keepBlock(content, v)
			case ToolResultBlock:
				nv, ch := ladderResult(v, rename)
				if !ch {
					content = keepBlock(content, v)
					continue
				}
				content = takeBlocks(content, m.Content, j)
				content = append(content, nv)
			default:
				content = keepBlock(content, b)
			}
		}

		if content == nil {
			if out != nil {
				out = append(out, m) // untouched message shares the input's array
			}
			continue
		}
		fork(i)
		if len(content) > 0 {
			m.Content = content
			out = append(out, m)
		}
		// else an emptied assistant message is dropped, as today
	}

	if out == nil {
		return msgs
	}
	return out
}

// degradeForeignBlocks strips text signatures and normalizes tool-call ids on a
// foreign message, recording rewrites in rename so its results follow. A
// same-origin message keeps its blocks. Both paths route every result through
// applyLadderToResults, which owns call id mapping and the placeholder ladder.
// Content clones only when a change lands; changed reports whether anything was
// rewritten.
func degradeForeignBlocks(m Message, caps Capabilities, target Origin, rename map[string]string) (Message, bool) {
	if !originForeign(m.Origin, target) {
		return applyLadderToResults(m, rename)
	}
	kind := callKindFor(m.Origin, target)

	var content BlockList // rebuilt blocks once a change lands
	for i, b := range m.Content {
		switch v := b.(type) {
		case TextBlock:
			if v.Signature == "" {
				content = keepBlock(content, b)
				continue
			}
			v.Signature = ""
			content = takeBlocks(content, m.Content, i)
			content = append(content, v)
		case ToolCallBlock:
			nid := normalizeCallID(v.ID, caps, target.Provider, kind)
			if nid == v.ID {
				content = keepBlock(content, b)
				continue
			}
			rename[v.ID] = nid
			v.ID = nid
			content = takeBlocks(content, m.Content, i)
			content = append(content, v)
		default:
			content = keepBlock(content, b)
		}
	}

	if content != nil {
		m.Content = content
	}
	laddered, ladderChanged := applyLadderToResults(m, rename)
	return laddered, content != nil || ladderChanged
}

// ladderResult rewrites one tool result for the target: its call id through
// rename and the placeholder ladder when no text survives. changed reports
// whether either applied.
func ladderResult(tr ToolResultBlock, rename map[string]string) (ToolResultBlock, bool) {
	nid := rename[tr.CallID]
	if nid == "" && trHasText(tr.Content) { // the ladder only pads when no text survives
		return tr, false
	}
	if nid != "" {
		tr.CallID = nid
	}
	return applyPlaceholderLadder(tr), true
}

// applyLadderToResults maps each result call id through the rename map and runs
// the placeholder ladder over every tool result. Content clones only when a
// change lands; changed reports whether anything was rewritten.
func applyLadderToResults(m Message, rename map[string]string) (Message, bool) {
	var content BlockList // rebuilt blocks once a change lands
	for i, b := range m.Content {
		tr, ok := b.(ToolResultBlock)
		if !ok {
			content = keepBlock(content, b)
			continue
		}
		nv, ch := ladderResult(tr, rename)
		if !ch {
			content = keepBlock(content, b)
			continue
		}
		content = takeBlocks(content, m.Content, i)
		content = append(content, nv)
	}
	if content == nil {
		return m, false
	}
	m.Content = content
	return m, true
}

// callKind classifies how an id crossed into the request: same endpoint, a model
// switch on this endpoint, or a foreign provider/dialect.
type callKind uint8

const (
	callSameEndpoint    callKind = iota // produced by the target itself
	callForeignModel                    // another model on the same endpoint
	callForeignEndpoint                 // different provider or dialect
)

// originForeign reports whether a message's provenance differs from target. Nil
// (unknown) counts as foreign.
func originForeign(o *Origin, target Origin) bool {
	return o == nil || *o != target
}

// endpointForeign reports whether the producing endpoint itself differs: provider
// or dialect mismatch. A model switch on the same endpoint is not foreign here.
func endpointForeign(o *Origin, target Origin) bool {
	if o == nil {
		return true
	}
	return o.Provider != target.Provider || o.Dialect != target.Dialect
}

// callKindFor classifies how a message's origin crossed into the request.
func callKindFor(o *Origin, target Origin) callKind {
	switch {
	case endpointForeign(o, target):
		return callForeignEndpoint
	case o.Model != target.Model:
		return callForeignModel
	default:
		return callSameEndpoint
	}
}

// applyPlaceholderLadder prepends a placeholder when a tool result has no text:
// "(see attached image)" when an image survives, else "(no tool output)".
func applyPlaceholderLadder(tr ToolResultBlock) ToolResultBlock {
	if trHasText(tr.Content) {
		return tr
	}
	p := "(no tool output)"
	if hasImage(tr.Content) {
		p = "(see attached image)"
	}
	out := make([]Block, 1, len(tr.Content)+1)
	out[0] = TextBlock{Text: p}
	tr.Content = append(out, tr.Content...)
	return tr
}

func trHasText(content BlockList) bool {
	return slices.ContainsFunc(content, func(b Block) bool {
		t, ok := b.(TextBlock)
		return ok && strings.TrimSpace(t.Text) != ""
	})
}

// repairTurns enforces request well-formedness: errored assistant turns are
// skipped along with their tool results (their results would be an unmatched
// tool_result Anthropic rejects), aborted ones stay because the abort path fills
// every pending call, and unanswered calls receive a synthetic error result.
// Empty ids drop and two calls sharing one id inside a batch keep only the
// first, the only shapes that still cannot pair. An id a later exchange reuses
// is renamed instead: distinct round trips are distinct history, and both ship.
// A list needing none of those repairs passes through untouched. The id maps
// stay eager: building them is the scan that detects a needed repair.
func repairTurns(msgs []Message, caps Capabilities) []Message {
	var out []Message // nil while every message passes through untouched
	// fork copies the messages already passed through on the first change, so a
	// clean pass returns the caller's slice and a dirty one owns its output
	fork := func(i int) {
		if out == nil {
			out = slices.Clone(msgs[:i])
		}
	}

	skippedCalls := map[string]struct{}{}
	pending := map[string]ToolCallBlock{}
	seenCalls := map[string]struct{}{} // every call id already emitted, answered or not
	answered := map[string]struct{}{}  // every call id a result already answered
	renamed := map[string]string{}     // reused id -> the id its latest exchange ships under
	var order []string
	var lastWasResults bool // whether the last emitted message was tool-results-only

	flushPending := func(i int) {
		if len(order) == 0 {
			return
		}
		content := make(BlockList, 0, len(order))
		for _, id := range order {
			c, ok := pending[id]
			if !ok {
				continue // already answered by a real result in this batch
			}
			delete(pending, id)
			content = append(content, ToolResultBlock{
				CallID: c.ID, ToolName: c.Name, IsError: true,
				Content: BlockList{TextBlock{Text: noResult}},
			})
		}
		order = nil
		if len(content) > 0 {
			fork(i)
			out = append(out, Message{Role: RoleUser, Content: content})
			lastWasResults = true // the synthesized results turn still needs a bridge
		}
	}

	for i, m := range msgs {
		if m.Role == RoleAssistant && m.Stop == StopError {
			for _, b := range m.Content {
				if tc, ok := b.(ToolCallBlock); ok && tc.ID != "" {
					skippedCalls[tc.ID] = struct{}{}
				}
			}
			fork(i) // the errored turn is dropped wholesale
			continue
		}

		// a new turn boundary flushes any still-unanswered prior calls first
		if m.Role == RoleAssistant || (m.Role == RoleUser && !OnlyToolResults(m.Content)) {
			flushPending(i)
		}

		// some chat-completions providers need an assistant reply between a tool-result
		// turn and the next user message. Every message recomputes whether it is one
		if caps.RequiresAssistantAfterToolResult && lastWasResults && m.Role == RoleUser {
			fork(i)
			out = append(out, Text(RoleAssistant, processedTools))
		}

		var content BlockList // rebuilt blocks once a change lands in this message
		for j, b := range m.Content {
			if tr, ok := b.(ToolResultBlock); ok {
				if tr.CallID == "" {
					content = takeBlocks(content, m.Content, j)
					continue // unpairable, dropped rather than sent dangling
				}
				if _, skip := skippedCalls[tr.CallID]; skip {
					content = takeBlocks(content, m.Content, j)
					continue // result of a skipped (errored) call
				}
				if nid := renamed[tr.CallID]; nid != "" {
					tr.CallID = nid // a reused exchange reads under its renamed id
					content = takeBlocks(content, m.Content, j)
				}
				if _, dup := answered[tr.CallID]; dup {
					content = takeBlocks(content, m.Content, j)
					continue // echo of an already-answered duplicate call
				}
				if c, inTurn := pending[tr.CallID]; inTurn {
					delete(pending, tr.CallID)
					answered[tr.CallID] = struct{}{}
					if tr.ToolName == "" && c.Name != "" {
						tr.ToolName = c.Name
						content = takeBlocks(content, m.Content, j)
					}
				}
				content = keepBlock(content, tr)
				continue
			}
			if tc, ok := b.(ToolCallBlock); ok {
				if tc.ID == "" {
					content = takeBlocks(content, m.Content, j)
					continue // unanswerable, dropped rather than sent dangling
				}
				// a live call re-owning the id frees an earlier errored turn's skip
				delete(skippedCalls, tc.ID)
				if _, dup := seenCalls[tc.ID]; dup {
					if _, open := pending[tc.ID]; open {
						content = takeBlocks(content, m.Content, j)
						continue // same batch: two calls cannot pair under one id, first stands
					}
					// a later exchange reusing an id an earlier one spent: rename so
					// both round trips ship, the first keeps the original
					nid := uniqueCallID(tc.ID, seenCalls)
					renamed[tc.ID] = nid
					tc.ID = nid
					content = takeBlocks(content, m.Content, j)
				}
				seenCalls[tc.ID] = struct{}{}
				pending[tc.ID] = tc
				order = append(order, tc.ID)
				content = keepBlock(content, tc)
				continue
			}
			content = keepBlock(content, b)
		}

		if content == nil && len(m.Content) == 0 {
			fork(i)
			continue // a message with nothing to send drops, as today
		}
		if content == nil {
			if out != nil {
				out = append(out, m) // untouched message shares the input's array
			}
			lastWasResults = m.Role == RoleUser && OnlyToolResults(m.Content)
			continue
		}
		fork(i)
		if len(content) == 0 {
			continue // a message emptied by skipped results is dropped
		}
		lastWasResults = m.Role == RoleUser && OnlyToolResults(content)
		// Stop rides along so a rebuilt turn matches what a clean pass would emit
		out = append(out, Message{Role: m.Role, Stop: m.Stop, Content: content})
	}

	flushPending(len(msgs))
	if out == nil {
		return msgs
	}
	return out
}

// RepairToolInputs returns m with every empty or invalid tool-call input
// replaced by {}, so any message carrying one marshals for both storage and
// the wire. Malformed arguments already failed their call at the stream
// boundary, and survive only in its paired error result.
func RepairToolInputs(m Message) Message {
	repaired, _ := repairCallInputs(m)
	return repaired
}

// repairToolInputs runs the input repair over a list, cloning lazily so the
// clean path shares the caller's backing arrays.
func repairToolInputs(msgs []Message) []Message {
	var out []Message
	for i, m := range msgs {
		r, changed := repairCallInputs(m)
		if !changed {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		out[i] = r
	}
	if out == nil {
		return msgs
	}
	return out
}

// repairCallInputs substitutes {} for each invalid input, reporting whether any
// block changed. Content clones lazily, only when a change lands.
func repairCallInputs(m Message) (Message, bool) {
	var content BlockList
	for i, b := range m.Content {
		tc, ok := b.(ToolCallBlock)
		if !ok || json.Valid(tc.Input) { // invalid covers nil and empty too
			continue
		}
		if content == nil {
			content = slices.Clone(m.Content)
		}
		tc.Input = json.RawMessage("{}")
		content[i] = tc
	}
	if content == nil {
		return m, false
	}
	m.Content = content
	return m, true
}

// synthCallIDPrefix leads every id synthesized for an absent tool-call id, so
// repairs stay recognizable in transcripts and request logs.
const synthCallIDPrefix = "synth_"

// RepairToolCallIDs returns m with every tool-call id that is empty, repeated
// in m, or already used by history rewritten to a unique one, and reports
// whether any id changed. History is every message the session already holds,
// so a rewritten id cannot collide with the transcript either. Only a unique
// id pairs a call with its own result on the wire.
func RepairToolCallIDs(m Message, history []Message) (Message, bool) {
	// ids history already spent, joined by each first occurrence in m
	taken := usedCallIDs(history)
	var content BlockList
	for i, b := range m.Content {
		tc, ok := b.(ToolCallBlock)
		if !ok {
			continue
		}
		if tc.ID != "" {
			if _, used := taken[tc.ID]; !used {
				taken[tc.ID] = struct{}{} // first occurrence stands
				continue
			}
		}
		if content == nil {
			content = slices.Clone(m.Content)
		}
		if tc.ID == "" {
			tc.ID = synthCallID(tc, i, taken) // absent id: nothing to keep, hash-name it
		} else {
			// repeated in m or reused from history: keep the id recognizable
			tc.ID = uniqueCallID(tc.ID, taken)
		}
		taken[tc.ID] = struct{}{}
		content[i] = tc
	}
	if content == nil {
		return m, false
	}
	m.Content = content
	return m, true
}

// usedCallIDs returns every tool-call id and result call id msgs holds.
func usedCallIDs(msgs []Message) map[string]struct{} {
	set := make(map[string]struct{})
	for _, m := range msgs {
		for _, b := range m.Content {
			switch v := b.(type) {
			case ToolCallBlock:
				if v.ID != "" {
					set[v.ID] = struct{}{}
				}
			case ToolResultBlock:
				if v.CallID != "" {
					set[v.CallID] = struct{}{}
				}
			}
		}
	}
	return set
}

// synthCallID returns an id for the tool call at index i that no entry of taken holds.
// Invalid input hashes as {}, matching the form the transcript persists, so the id
// stays derivable from stored state.
func synthCallID(tc ToolCallBlock, i int, taken map[string]struct{}) string {
	input := tc.Input
	if !json.Valid(input) {
		input = json.RawMessage("{}")
	}
	base := synthCallIDPrefix + shortHash(tc.Name+"\x00"+string(input)+"\x00"+strconv.Itoa(i))
	return uniqueCallID(base, taken)
}

// uniqueCallID returns base, or base with the first free _N suffix, so a
// rewritten id never collides with one taken already holds.
func uniqueCallID(base string, taken map[string]struct{}) string {
	id := base
	for n := 2; ; n++ {
		if _, ok := taken[id]; !ok {
			return id
		}
		id = base + "_" + strconv.Itoa(n)
	}
}

// normalizeCallID returns id in the form target dialect accepts. kind marks how far the source
// crossed: a same-endpoint model switch drops an fc_ item, while a foreign endpoint hashes it.
func normalizeCallID(id string, caps Capabilities, provider string, kind callKind) string {
	switch caps.Dialect {
	case DialectAnthropic:
		return cutID(sanitizeID(id), 64)
	case DialectOpenAIResponses:
		return responsesCallID(id, kind)
	default: // chat-completions
		if callID, itemID, ok := strings.Cut(id, "|"); ok {
			nid := sanitizeID(callID + "_" + itemID)
			if len(nid) <= 40 {
				return nid
			}
			return cutID(sanitizeID(callID), 31) + "_" + shortHash(id)[:8]
		}
		// openai caps a bare tool_call_id, while other providers accept longer ids verbatim
		if provider == providerOpenAI && len(id) > maxCompatCallID {
			return cutID(sanitizeID(id), maxCompatCallID)
		}
		return id
	}
}

// responsesCallID normalizes a piped Responses call id for the dialect.
func responsesCallID(id string, kind callKind) string {
	callID, itemID, ok := strings.Cut(id, "|")
	if !ok {
		return normalizeIDPart(id)
	}
	switch kind {
	case callForeignEndpoint:
		// the raw unsanitized item id is what gets hashed
		return normalizeIDPart(callID) + "|fc_" + shortHash(itemID)
	default: // same endpoint, a non-fc or cross-model id is prefixed then dropped at
		// emit time, so dropping here makes the wire result identical to that shape
		if !strings.HasPrefix(itemID, "fc_") || kind == callForeignModel {
			return normalizeIDPart(callID)
		}
		item := normalizeIDPart(itemID)
		return normalizeIDPart(callID) + "|" + item
	}
}

// normalizeIDPart sanitizes an id to [a-zA-Z0-9_-], caps it at 64 and trims a
// trailing underscore.
func normalizeIDPart(id string) string {
	return strings.TrimRight(cutID(sanitizeID(id), 64), "_")
}

// sanitizeID maps non [a-zA-Z0-9_-] runes to an underscore.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if isAllowedIDRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func isAllowedIDRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' || r == '-' || r == '_'
}

// cutID cuts s to at most n bytes, dropping a partial trailing rune.
func cutID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// utf8RuneStart reports whether the byte is a UTF-8 rune start.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// shortHash returns an FNV-64a base36 digest of s, for ids that must be bounded and stable within
// ajent. Only determinism matters here, and the exact hash function is free to change.
func shortHash(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 36)
}
