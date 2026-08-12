// Package tokens estimates and accounts token usage without a vendored BPE
// tokenizer. Exact counts come from llm.Counter where the provider has one; this
// package supplies the heuristic estimate that fills between exact reports.
package tokens

import (
	"strings"
	"unicode/utf8"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Kind is a content class with its own bytes-per-token ratio.
type Kind int

const (
	KindProse Kind = iota
	KindCode
	KindJSON
)

// bytesPerToken by kind: prose 4, code 3.2, JSON 2.8.
var bytesPerToken = map[Kind]float64{
	KindProse: 4.0,
	KindCode:  3.2,
	KindJSON:  2.8,
}

const (
	// messageOverhead is the framing a provider adds around each message.
	messageOverhead = 6
	// schemaOverhead rides with tool schemas beyond their JSON size.
	schemaOverhead = 12
	// imageBaseTokens is a floor for an image block of unknown dimensions.
	imageBaseTokens = 85
	// imageBytesPerToken approximates the byte cost of an encoded image.
	imageBytesPerToken = 512
)

// EstimateText estimates the token count of text under kind. ASCII bytes divide
// by the ratio; each non-ASCII rune counts one token, plus one more for astral
// (emoji) pairs above U+FFFF.
func EstimateText(text string, kind Kind) int {
	ratio := bytesPerToken[kind]
	if ratio <= 0 { // unknown kind: fall back to prose rather than divide by zero
		ratio = bytesPerToken[KindProse]
	}
	var ascii int
	tokens := 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			tokens++ // one token per non-ASCII rune (CJK, accented)
			if r > 0xFFFF {
				tokens++ // astral pairs cost two
			}
		}
	}
	return tokens + int(float64(ascii)/ratio)
}

// estimateBlocks returns the content tokens of blocks. Text inside a tool result
// reads as code, since file contents dominate an agent context.
func estimateBlocks(blocks llm.BlockList) int {
	var n int
	for _, blk := range blocks {
		switch b := blk.(type) {
		case llm.TextBlock:
			n += EstimateText(b.Text, KindProse)
		case llm.ThinkingBlock:
			n += EstimateText(b.Text, KindProse)
		case llm.ToolCallBlock:
			n += estimateToolCall(b)
		case llm.ImageBlock:
			n += imageTokens(len(b.Data))
		case llm.ToolResultBlock:
			for _, cb := range b.Content {
				if tb, ok := cb.(llm.TextBlock); ok {
					n += EstimateText(tb.Text, KindCode)
				} else {
					n += estimateBlocks(llm.BlockList{cb})
				}
			}
		}
	}
	return n
}

// estimateToolCall counts the JSON arguments plus a small name/id overhead.
func estimateToolCall(b llm.ToolCallBlock) int {
	const idOverhead = 6
	return EstimateText(string(b.Input), KindJSON) + idOverhead
}

// imageTokens is a size-derived constant; images are unknowable without
// dimensions, so this only has to be roughly right.
func imageTokens(n int) int {
	t := n / imageBytesPerToken
	if t < imageBaseTokens {
		return imageBaseTokens
	}
	return t
}

// EstimateMessage estimates a single message including its per-message framing.
func EstimateMessage(m llm.Message) int {
	return messageOverhead + estimateBlocks(m.Content)
}

// EstimateMessages estimates one or more messages including per-message framing.
func EstimateMessages(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += EstimateMessage(m)
	}
	return n
}

// EstimateRequest estimates the whole prompt a provider will receive: system,
// retained messages (thinking already resolved by retention) and tool schemas.
func EstimateRequest(req llm.Request) int {
	n := 0
	for _, blk := range req.System {
		if tb, ok := blk.(llm.TextBlock); ok {
			n += EstimateText(tb.Text, KindProse)
		}
	}
	for _, m := range llm.RetainedMessages(req) {
		n += messageOverhead + estimateBlocks(m.Content)
	}
	if len(req.Tools) > 0 {
		var t strings.Builder
		for _, s := range req.Tools {
			t.WriteString(s.Name)
			t.Write(s.Parameters)
		}
		n += EstimateText(t.String(), KindJSON) + schemaOverhead*len(req.Tools)
	}
	return n
}
