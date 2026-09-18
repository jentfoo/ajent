package mcp

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// Result is a bridged tool call's outcome before mapping onto agent.ToolResult.
type Result struct {
	Blocks  []llm.Block // model-visible content, in arrival order
	IsError bool
}

// mapCallResult converts an mcp CallToolResult into our own Result. Images
// pass through as image blocks when they carry bytes, else a short placeholder;
// the registry's result normalization bounds and gates whatever lands. Audio
// and the rest map to plain text.
func mapCallResult(r *mcp.CallToolResult) Result {
	res := Result{IsError: r.IsError}
	for _, c := range r.Content {
		switch b := c.(type) {
		case mcp.TextContent:
			if t := strings.TrimSpace(b.Text); t != "" {
				res.Blocks = append(res.Blocks, llm.TextBlock{Text: t})
			}
		case mcp.ImageContent:
			if data, ok := imageBytes(b.MIMEType, b.Data); ok {
				res.Blocks = append(res.Blocks, llm.ImageBlock{MediaType: b.MIMEType, Data: data})
				continue
			}
			mime := b.MIMEType
			if mime == "" {
				mime = "image"
			}
			res.Blocks = append(res.Blocks, llm.TextBlock{Text: fmt.Sprintf("[image omitted: %s]", mime)})
		case mcp.AudioContent:
			mime := b.MIMEType
			if mime == "" {
				mime = "audio"
			}
			res.Blocks = append(res.Blocks, llm.TextBlock{Text: fmt.Sprintf("[audio result: %d bytes (%s)]", len(b.Data), mime)})
		default:
			res.Blocks = append(res.Blocks, llm.TextBlock{Text: refText(c)})
		}
	}
	if len(r.RawStructuredContent) > 0 && len(res.Blocks) == 0 {
		res.Blocks = append(res.Blocks, llm.TextBlock{Text: string(r.RawStructuredContent)})
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

// toBlocks returns the result's model-visible content.
func (r Result) toBlocks() llm.BlockList { return r.Blocks }

// imageBytes decodes an mcp image payload's base64 data, ok false for a
// non-image mime or nothing decodable. Padded and unpadded, standard and
// URL-safe alphabets all appear in the wild.
func imageBytes(mime, data string) ([]byte, bool) {
	if !strings.HasPrefix(mime, "image/") || data == "" {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(data); err == nil && len(b) > 0 {
			return b, true
		}
	}
	return nil, false
}

// displayOf renders a one-line history summary of the result.
func displayOf(r Result) string {
	var sb strings.Builder
	for _, b := range r.Blocks {
		switch v := b.(type) {
		case llm.TextBlock:
			sb.WriteString(v.Text)
		case llm.ImageBlock:
			sb.WriteString(llm.ImagePlaceholder(v.Data, ""))
		default:
			sb.WriteString("[content]")
		}
	}
	return strutil.Clip(sb.String(), 1000)
}
