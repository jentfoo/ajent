package llm

import (
	"context"
	"errors"
	"strings"
)

// ErrTruncated reports a response the provider stopped at its output token cap.
// Its text is partial and must never be persisted or acted on.
var ErrTruncated = errors.New("generation stopped at the output token cap: the response is incomplete")

// RunSummary drives one model call through an accumulator and returns its
// assistant text plus the usage the provider reported, including whatever a
// failed call already billed after the stream opened. A response stopped at
// the output token cap is an error, never partial text.
func RunSummary(ctx context.Context, p Provider, req Request) (string, Usage, error) {
	st, err := p.Stream(ctx, req)
	if err != nil {
		return "", Usage{}, err
	}
	defer func() { _ = st.Close() }()
	stop := CloseOnDone(ctx, st)
	defer stop()

	var acc Accumulator
	for ev, ok := st.Next(); ok; ev, ok = st.Next() {
		acc.Add(ev)
		if ctx.Err() != nil {
			return "", acc.Usage(), ctx.Err() // cancelled: stop consuming the response
		}
	}
	if err := ctx.Err(); err != nil {
		return "", acc.Usage(), err // deliberate close leaves st.Err nil, never return partial text
	}
	if err := st.Err(); err != nil {
		return "", acc.Usage(), err
	}
	if err := acc.Err(); err != nil {
		return "", acc.Usage(), err
	}
	if acc.StopReason() == StopMaxTokens {
		return "", acc.Usage(), ErrTruncated
	}

	var sb strings.Builder
	for _, b := range acc.Message().Content {
		if tb, ok := b.(TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String(), acc.Usage(), nil
}

// LastAssistantMessage returns the newest assistant message in msgs, or nil
// when the context holds none.
func LastAssistantMessage(msgs []Message) *Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			return &msgs[i]
		}
	}
	return nil
}

// JoinText joins a block list's non-blank text blocks with newlines. Thinking
// and tool blocks never contribute.
func JoinText(blocks BlockList) string {
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteRune('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// FinalAnswer returns the joined text of the last assistant message's non-blank
// blocks, or "" when the turn produced none.
func FinalAnswer(msgs []Message) string {
	m := LastAssistantMessage(msgs)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(JoinText(m.Content))
}

// LastAssistantText returns the text of the newest assistant message that has
// any, empty when the context holds none.
func LastAssistantText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleAssistant {
			continue
		}

		var b strings.Builder
		for _, blk := range msgs[i].Content {
			if tb, ok := blk.(TextBlock); ok {
				b.WriteString(tb.Text)
			}
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			return text
		}
	}
	return ""
}

// FirstBlockText returns the first non-blank text block in a list, or "".
func FirstBlockText(blocks BlockList) string {
	for _, b := range blocks {
		if tb, ok := b.(TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			return tb.Text
		}
	}
	return ""
}
