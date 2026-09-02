package llm

import (
	"context"
	"errors"
	"strings"
)

// ErrTruncated reports a response the provider stopped at its output token cap;
// its text is partial and must never be persisted or acted on.
var ErrTruncated = errors.New("generation stopped at the output token cap: the response is incomplete")

// RunSummary drives one model call through an accumulator and returns its
// assistant text plus the usage the provider reported. A response the provider
// stopped at its output token cap is an error, never partial text.
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
			return "", Usage{}, ctx.Err() // cancelled: stop consuming the response
		}
	}
	if err := ctx.Err(); err != nil {
		return "", Usage{}, err // deliberate close leaves st.Err nil; never return partial text
	}
	if err := st.Err(); err != nil {
		return "", Usage{}, err
	}
	if err := acc.Err(); err != nil {
		return "", Usage{}, err
	}
	if acc.StopReason() == StopMaxTokens {
		return "", Usage{}, ErrTruncated
	}

	var parts []string
	for _, b := range acc.Message().Content {
		if tb, ok := b.(TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, ""), acc.Usage(), nil
}

// FinalAnswer returns the joined text of the last assistant message's non-blank
// blocks, or "" when the turn produced none.
func FinalAnswer(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleAssistant {
			continue
		}
		var parts []string
		for _, b := range msgs[i].Content {
			if tb, ok := b.(TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
				parts = append(parts, tb.Text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
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
