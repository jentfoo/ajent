// Package tokens estimates and accounts token usage without a vendored BPE
// tokenizer. Exact counts come from llm.Counter where the provider has one; this
// package supplies the heuristic estimate that fills between exact reports.
package tokens

import (
	"encoding/binary"
	"math"
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
	// KindOpaque is base64 and other machine payloads replayed verbatim, which
	// tokenize coarser than the JSON that carries them.
	KindOpaque
)

// bytesPerToken by kind: prose 4, code 3.2, JSON 2.8, opaque 3.6.
var bytesPerToken = map[Kind]float64{
	KindProse:  4.0,
	KindCode:   3.2,
	KindJSON:   2.8,
	KindOpaque: 3.6,
}

const (
	// messageOverhead is the framing a provider adds around each message.
	messageOverhead = 6
	// schemaOverhead rides with tool schemas beyond their JSON size.
	schemaOverhead = 12
	// toolCallOverhead is the type/id/name framing around a call's arguments.
	toolCallOverhead = 8
	// toolResultOverhead is the type/tool_use_id framing around a result's content.
	toolResultOverhead = 10
	// unknownBlockTokens is charged for a block this estimator has no arm for.
	// llm.Block is sealed, so it only fires when a type lands in pkg/llm without
	// one here: a small constant keeps that visible as calibration drift without
	// paying to marshal anything on the hot path.
	unknownBlockTokens = 16
	// imageBaseTokens is a floor for an image block of unknown dimensions.
	imageBaseTokens = 85
	// imageMaxTokens caps a single image, as the providers do after downscaling.
	imageMaxTokens = 1600
	// imagePixelsPerToken is the area a token covers once dimensions are known.
	imagePixelsPerToken = 750
	// imageBytesPerToken approximates the byte cost of an encoded image, for
	// formats whose header does not give dimensions.
	imageBytesPerToken = 512
)

// EstimateBytes estimates the token count of n bytes of ASCII content under
// kind. Use it to size content that has been measured but not read.
func EstimateBytes(n int64, kind Kind) int {
	if n <= 0 {
		return 0
	}
	ratio := bytesPerToken[kind]
	if ratio <= 0 { // unknown kind: fall back to prose rather than divide by zero
		ratio = bytesPerToken[KindProse]
	}
	return int(math.Round(float64(n) / ratio))
}

// EstimateText estimates the token count of text under kind. ASCII bytes divide
// by the ratio; each non-ASCII rune counts one token, plus one more for astral
// (emoji) pairs above U+FFFF.
func EstimateText(text string, kind Kind) int {
	var ascii int64
	var tokens int
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
	return tokens + EstimateBytes(ascii, kind)
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
			n += estimateThinking(b)
		case llm.ToolCallBlock:
			n += estimateToolCall(b)
		case llm.ImageBlock:
			n += imageTokens(b)
		case llm.ToolResultBlock:
			n += estimateToolResult(b)
		default:
			n += unknownBlockTokens
		}
	}
	return n
}

// estimateThinking sizes a thinking block by the one payload its provider
// replays. A serialized Responses item supersedes the text and encrypted content
// it already embeds, and Anthropic sends redacted data in place of thinking, so
// these are alternatives rather than a sum.
func estimateThinking(b llm.ThinkingBlock) int {
	switch {
	case len(b.Item) > 0:
		return EstimateText(string(b.Item), KindOpaque)
	case b.Redacted != "":
		return EstimateText(b.Redacted, KindOpaque)
	}
	return EstimateText(b.Text, KindProse) +
		EstimateText(b.Signature, KindOpaque) +
		EstimateText(b.Encrypted, KindOpaque) +
		EstimateText(string(b.Details), KindJSON)
}

// estimateToolCall counts the JSON arguments plus the name and id that frame them.
func estimateToolCall(b llm.ToolCallBlock) int {
	return EstimateText(string(b.Input), KindJSON) +
		EstimateText(b.Name, KindProse) +
		EstimateText(b.ID, KindOpaque) + toolCallOverhead
}

// estimateToolResult counts a result's content plus the id that binds it to its
// call. Display, Details and AddedToolNames are transcript and extension fields
// that never reach the wire, so they are deliberately excluded.
func estimateToolResult(b llm.ToolResultBlock) int {
	n := toolResultOverhead + EstimateText(b.CallID, KindOpaque) +
		EstimateText(b.ToolName, KindProse)
	for _, cb := range b.Content {
		if tb, ok := cb.(llm.TextBlock); ok {
			n += EstimateText(tb.Text, KindCode)
		} else {
			n += estimateBlocks(llm.BlockList{cb})
		}
	}
	return n
}

// imageTokens sizes an image from its pixel dimensions where the header carries
// them, since cost scales with area rather than with how well it compressed. A
// format whose header does not parse falls back to a byte ratio.
func imageTokens(b llm.ImageBlock) int {
	if w, h, ok := imageDims(b.Data); ok {
		return min(max(w*h/imagePixelsPerToken, imageBaseTokens), imageMaxTokens)
	}
	return max(len(b.Data)/imageBytesPerToken, imageBaseTokens)
}

var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// imageDims returns the pixel dimensions encoded in a PNG or JPEG header. It
// reads the header only; nothing is decoded. ok is false for any other format.
func imageDims(data []byte) (w, h int, ok bool) {
	switch {
	case len(data) >= 24 && string(data[:8]) == string(pngMagic):
		// IHDR is always the first chunk: width and height are big endian at 16 and 20
		return int(binary.BigEndian.Uint32(data[16:20])),
			int(binary.BigEndian.Uint32(data[20:24])), true
	case len(data) >= 4 && data[0] == 0xFF && data[1] == 0xD8:
		return jpegDims(data)
	}
	return 0, 0, false
}

// jpegDims walks JPEG segment markers to the start-of-frame that carries the
// dimensions, skipping every other segment by its length.
func jpegDims(data []byte) (w, h int, ok bool) {
	for i := 2; i+9 < len(data); {
		if data[i] != 0xFF {
			return 0, 0, false // out of step with the segment chain
		}
		marker := data[i+1]
		if marker == 0xFF {
			i++ // fill byte, the marker continues
			continue
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 {
			return 0, 0, false
		}
		// SOF0-SOF15 carry the frame header; DHT (C4), JPG (C8) and DAC (CC) do not
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			return int(binary.BigEndian.Uint16(data[i+7 : i+9])),
				int(binary.BigEndian.Uint16(data[i+5 : i+7])), true
		}
		i += 2 + size
	}
	return 0, 0, false
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

// EstimateFor estimates the messages a request to m would carry, applying the
// same llm.Prepare pass the wire does so thinking the request would drop is never
// counted as context. A zero Model skips Prepare: with no capabilities to
// normalize against it would degrade every message as foreign and downgrade every
// image, which is a worse guess than the raw messages. Fixed request overhead is
// excluded; callers hold that separately.
func EstimateFor(m llm.Model, retain llm.RetainPolicy, msgs []llm.Message) int {
	if m.ID == "" {
		return EstimateMessages(msgs)
	}
	req := llm.Prepare(llm.Request{
		Model:     m,
		Messages:  msgs,
		Reasoning: llm.ReasoningConfig{Retain: retain},
	})
	return EstimateMessages(req.Messages)
}

// EstimateToolPair sizes the synthetic tool-call + result pair an injected read
// or listing produces: both messages' framing, the call's arguments, and body
// bytes of result content under kind. Callers reserve this before running the
// call, so it must stay in step with what EstimateMessages bills for the same
// pair once it lands. The result side counts no tool name because agent.InjectPair
// leaves it unset; llm.Prepare fills it in at request build time, which is past
// what the ledger bills.
func EstimateToolPair(call llm.ToolCallBlock, body int64, kind Kind) int {
	return 2*messageOverhead + estimateToolCall(call) +
		toolResultOverhead + EstimateText(call.ID, KindOpaque) + EstimateBytes(body, kind)
}

// EstimateFixed estimates the message-independent part of a request: system
// blocks and tool schemas. Messages are excluded because callers account them as
// they append, so this is what a fresh ledger still owes once its messages land.
func EstimateFixed(req llm.Request) int {
	n := 0
	for _, blk := range req.System {
		if tb, ok := blk.(llm.TextBlock); ok {
			n += EstimateText(tb.Text, KindProse)
		} else {
			n += unknownBlockTokens // a non-text system block still occupies context
		}
	}
	if len(req.Tools) > 0 {
		// descriptions are prose and often the larger half of a schema, so they are
		// measured apart from the JSON they ride in rather than at the JSON ratio
		var j, d strings.Builder
		for _, s := range req.Tools {
			j.WriteString(s.Name)
			j.Write(s.Parameters)
			d.WriteString(s.Description)
		}
		n += EstimateText(j.String(), KindJSON) + EstimateText(d.String(), KindProse) +
			schemaOverhead*len(req.Tools)
	}
	return n
}

// EstimateRequest estimates the whole prompt a provider will receive: system,
// retained messages (thinking already resolved by retention) and tool schemas.
func EstimateRequest(req llm.Request) int {
	n := EstimateFixed(req)
	req = llm.Prepare(req)
	return n + EstimateMessages(req.Messages)
}
