package mcp

import (
	"fmt"
	"strings"

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
			if t := strings.TrimSpace(b.Text); t != "" {
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
		return uriRef(b.URI, b.Description)
	case mcp.EmbeddedResource:
		return resourceRef(b.Resource)
	default:
		return "[content of unrecognized type]"
	}
}

// uriRef renders a reference as [resource: uri (label)].
func uriRef(uri, label string) string {
	if strings.TrimSpace(label) != "" {
		return fmt.Sprintf("[resource: %s (%s)]", uri, label)
	}
	return "[resource: " + uri + "]"
}

// resourceRef renders an embedded resource by its uri and mime type.
func resourceRef(r mcp.ResourceContents) string {
	switch b := r.(type) {
	case mcp.TextResourceContents:
		return uriRef(b.URI, b.MIMEType)
	case mcp.BlobResourceContents:
		return uriRef(b.URI, b.MIMEType)
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
