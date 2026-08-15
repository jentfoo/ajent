package mcp

import (
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Result is a bridged tool call's outcome before mapping onto agent.ToolResult.
type Result struct {
	Content []string // model-visible text blocks, in arrival order
	IsError bool
}

// mapCallResult converts an mcp CallToolResult into our own Result. Image and
// audio content become a short placeholder; the rest maps to plain text.
func mapCallResult(r *mcp.CallToolResult) Result {
	res := Result{IsError: r.IsError}
	for _, c := range r.Content {
		switch b := c.(type) {
		case mcp.TextContent:
			if t := stringsTrim(b.Text); t != "" {
				res.Content = append(res.Content, t)
			}
		case mcp.ImageContent:
			mime := b.MIMEType
			if mime == "" {
				mime = "image"
			}
			res.Content = append(res.Content, fmt.Sprintf("[image omitted: %s]", mime))
		case mcp.AudioContent:
			mime := b.MIMEType
			if mime == "" {
				mime = "audio"
			}
			res.Content = append(res.Content, fmt.Sprintf("[audio result: %d bytes (%s)]", len(b.Data), mime))
		default:
			res.Content = append(res.Content, refText(c))
		}
	}
	if len(r.RawStructuredContent) > 0 && len(res.Content) == 0 {
		res.Content = append(res.Content, string(r.RawStructuredContent))
	}
	return res
}

// refText renders a resource or embedded content as a text reference.
func refText(c mcp.Content) string {
	switch b := c.(type) {
	case mcp.ResourceLink:
		if stringsTrim(b.Description) != "" {
			return fmt.Sprintf("[resource: %s (%s)]", b.URI, b.Description)
		}
		return "[resource: " + b.URI + "]"
	default:
		return "[content of unrecognized type]"
	}
}

// toBlocks flattens a Result's content into an llm.BlockList for the model.
func (r Result) toBlocks() llm.BlockList {
	var out llm.BlockList
	for _, t := range r.Content {
		out = append(out, llm.TextBlock{Text: t})
	}
	return out
}

// stringsTrim trims ASCII whitespace from both ends of s.
func stringsTrim(s string) string {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	j := len(s)
	for j > i && isSpace(s[j-1]) {
		j--
	}
	return s[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
