package llm

import (
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
// legal and unanswered calls answered. Every request path passes through it so
// the estimator and the wire never disagree about what is sent. It is
// idempotent, so calling it twice yields an identical message list.
func Prepare(req Request) Request {
	caps := req.Model.Caps
	target := Origin{Provider: req.Model.Provider, Dialect: caps.Dialect, Model: req.Model.ID}

	msgs := downgradeImages(req.Messages, caps)
	msgs = splitInlineThinking(msgs, caps)
	msgs = normalizeContent(msgs, req.Reasoning.Retain, caps, target)
	msgs = splitToolResultImages(msgs, caps) // B4: compat carries tool images separately
	msgs = repairTurns(msgs, caps)

	// stamp the resolved origin so a later Prepare pass sees same-model messages
	for i := range msgs {
		if m := msgs[i]; m.Origin == nil || *m.Origin != target {
			m.Origin = &target
			msgs[i] = m
		}
	}
	req.Messages = msgs
	return req
}

// splitInlineThinking recovers reasoning that leaked into an assistant text
// block: chat-completions models mark reasoning with inline tags, and when a
// turn desyncs mid stream the tail plus its close tag can arrive on the content
// channel. Servers that render history through the model chat template read
// those markers themselves, and one stray tag makes them re-emit the message for
// every later assistant turn, multiplying the prompt far past what the
// estimator counted. The recovered block then flows through the normal
// retention, degradation and replay handling, so each model gets what it needs.
// The transcript itself stays verbatim, only the request copy is restructured.
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
	for _, b := range content {
		if tb, ok := b.(TextBlock); ok {
			if strings.Contains(tb.Text, open) || strings.Contains(tb.Text, close) {
				return true
			}
		}
	}
	return false
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
		var content BlockList
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
	for _, m := range msgs {
		if slices.ContainsFunc(m.Content, blockHasImage) {
			return true
		}
	}
	return false
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
	var out BlockList
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

// normalizeContent is the merged retention and cross-model degradation pass. Per-message it computes
// keep from the policy and foreign from whether its origin matches target, then rewrites blocks once.
func normalizeContent(msgs []Message, policy RetainPolicy, caps Capabilities, target Origin) []Message {
	policy = resolveRetention(policy, caps)
	keepFrom, lastAssistant := retentionBounds(msgs)

	out := make([]Message, 0, len(msgs))
	rename := map[string]string{} // original foreign call id -> normalized

	for i, m := range msgs {
		if m.Role != RoleAssistant || !hasThinking(m.Content) {
			nm := degradeForeignBlocks(m, caps, target, rename)
			out = append(out, nm)
			continue
		}

		keep := keepThinking(policy, i, keepFrom, lastAssistant)
		foreign := originForeign(m.Origin, target) // full mismatch: any model switch
		kind := callKindFor(m.Origin, target)      // endpoint vs same-endpoint for ids

		var content BlockList
		for _, b := range m.Content {
			switch v := b.(type) {
			case ThinkingBlock:
				if !keep || foreign && (v.Redacted != "" || strings.TrimSpace(v.Text) == "") {
					continue // dropped: policy, or an un-replayable foreign token
				}
				if foreign {
					// a foreign reasoning block degrades to visible text so the turn survives
					content = append(content, TextBlock{Text: v.Text})
					continue
				}
				switch {
				case caps.RequiresThinkingAsText:
					if strings.TrimSpace(v.Text) != "" {
						content = append(content, TextBlock{Text: v.Text})
					} else if replayable(v, caps) && (v.Signature != "" || v.Redacted != "") {
						content = append(content, v)
					}
				case !replayable(v, caps):
					continue // dropped: no token this provider accepts
				default:
					if strings.TrimSpace(v.Text) == "" && v.Signature == "" && v.Redacted == "" &&
						v.ItemID == "" && len(v.Item) == 0 && len(v.Details) == 0 {
						continue // nothing to replay at all
					}
					content = append(content, v)
				}
			case TextBlock:
				if foreign && v.Signature != "" {
					v.Signature = ""
				}
				content = append(content, v)
			case ToolCallBlock:
				if kind != callSameEndpoint {
					nid := normalizeCallID(v.ID, caps, target.Provider, kind)
					if nid != v.ID {
						rename[v.ID] = nid
						v.ID = nid
					}
				}
				content = append(content, v)
			case ToolResultBlock:
				if nid := rename[v.CallID]; nid != "" {
					v.CallID = nid
				}
				content = append(content, applyPlaceholderLadder(v))
			default:
				content = append(content, b)
			}
		}

		if len(content) == 0 {
			continue // an emptied assistant message is dropped, as today
		}
		m.Content = content
		out = append(out, m)
	}

	return out
}

// degradeForeignBlocks rewrites a message's foreign blocks: text signatures are
// dropped and tool-call ids normalized with the matching result rewritten in step.
func degradeForeignBlocks(m Message, caps Capabilities, target Origin, rename map[string]string) Message {
	out := slices.Clone(m.Content)
	if !originForeign(m.Origin, target) {
		m.Content = out
		return applyLadderToResults(m, rename)
	}
	kind := callKindFor(m.Origin, target)
	for i, b := range out {
		switch v := b.(type) {
		case TextBlock:
			if v.Signature != "" {
				v.Signature = ""
				out[i] = v
			}
		case ToolCallBlock:
			nid := normalizeCallID(v.ID, caps, target.Provider, kind)
			if nid != v.ID {
				rename[v.ID] = nid
				v.ID = nid
				out[i] = v
			}
		case ToolResultBlock:
			if nid := rename[v.CallID]; nid != "" {
				v.CallID = nid
				out[i] = v
			}
		}
	}
	m.Content = out
	return applyLadderToResults(m, rename)
}

// applyLadderToResults maps each result call id through the rename map and runs
// the placeholder ladder over every tool result.
func applyLadderToResults(m Message, rename map[string]string) Message {
	for i, b := range m.Content {
		if tr, ok := b.(ToolResultBlock); ok {
			if nid := rename[tr.CallID]; nid != "" {
				tr.CallID = nid
			}
			m.Content[i] = applyPlaceholderLadder(tr)
		}
	}
	return m
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
	for _, b := range content {
		if t, ok := b.(TextBlock); ok && strings.TrimSpace(t.Text) != "" {
			return true
		}
	}
	return false
}

// repairTurns enforces request well-formedness: errored assistant turns are
// skipped along with their tool results (their results would be an unmatched
// tool_result Anthropic rejects), aborted ones stay because the abort path fills
// every pending call, and unanswered calls receive a synthetic error result.
func repairTurns(msgs []Message, caps Capabilities) []Message {
	var out []Message
	skippedCalls := map[string]struct{}{}
	pending := map[string]ToolCallBlock{}
	var order []string
	var lastWasResults bool // whether the last emitted message was tool-results-only

	flushPending := func() {
		if len(order) == 0 {
			return
		}
		var content BlockList
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
			out = append(out, Message{Role: RoleUser, Content: content})
			lastWasResults = true // the synthesized results turn still needs a bridge
		}
	}

	for _, m := range msgs {
		if m.Role == RoleAssistant && m.Stop == StopError {
			for _, b := range m.Content {
				if tc, ok := b.(ToolCallBlock); ok && tc.ID != "" {
					skippedCalls[tc.ID] = struct{}{}
				}
			}
			continue // the errored turn is dropped wholesale
		}

		// a new turn boundary flushes any still-unanswered prior calls first
		if m.Role == RoleAssistant || (m.Role == RoleUser && !OnlyToolResults(m.Content)) {
			flushPending()
		}

		// some chat-completions providers need an assistant reply between a tool-result
		// turn and the next user message. Every message recomputes whether it is one
		if caps.RequiresAssistantAfterToolResult && lastWasResults && m.Role == RoleUser {
			out = append(out, Text(RoleAssistant, processedTools))
		}

		var content BlockList
		for _, b := range m.Content {
			if tr, ok := b.(ToolResultBlock); ok {
				if _, skip := skippedCalls[tr.CallID]; skip {
					continue // result of a skipped (errored) call
				}
				if c, inTurn := pending[tr.CallID]; inTurn {
					delete(pending, tr.CallID)
					if tr.ToolName == "" && c.Name != "" {
						tr.ToolName = c.Name
					}
				}
				content = append(content, tr)
				continue
			}
			if tc, ok := b.(ToolCallBlock); ok && tc.ID != "" {
				pending[tc.ID] = tc
				order = append(order, tc.ID)
			}
			content = append(content, b)
		}

		if len(content) == 0 {
			continue // a message emptied by skipped results is dropped
		}
		lastWasResults = m.Role == RoleUser && OnlyToolResults(content)
		out = append(out, Message{Role: m.Role, Content: content})
	}

	flushPending()
	return out
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
