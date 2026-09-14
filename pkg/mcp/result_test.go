package mcp

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"

	"github.com/jentfoo/ajent/pkg/llm"
)

func TestMapCallResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		result  *mcp.CallToolResult
		want    []string
		isError bool
	}{
		{
			name:   "text_content_is_trimmed",
			result: &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Text: "  hello  "}}},
			want:   []string{"hello"},
		},
		{
			name:   "empty_text_is_skipped",
			result: &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Text: "   "}}},
		},
		{
			name:   "image_becomes_placeholder",
			result: &mcp.CallToolResult{Content: []mcp.Content{mcp.ImageContent{MIMEType: "image/png"}}},
			want:   []string{"[image omitted: image/png]"},
		},
		{
			name:   "audio_becomes_placeholder",
			result: &mcp.CallToolResult{Content: []mcp.Content{mcp.AudioContent{MIMEType: "audio/wav", Data: "x"}}},
			want:   []string{"[audio result: 1 bytes (audio/wav)]"},
		},
		{
			name: "resource_link_with_description",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				mcp.ResourceLink{URI: "file:///a", Description: "notes"},
			}},
			want: []string{"[resource: file:///a (notes)]"},
		},
		{
			name: "resource_link_without_description",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				mcp.ResourceLink{URI: "file:///a"},
			}},
			want: []string{"[resource: file:///a]"},
		},
		{
			name: "embedded_text_resource_renders",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				mcp.EmbeddedResource{Resource: mcp.TextResourceContents{URI: "file:///b", MIMEType: "text/markdown"}},
			}},
			want: []string{"[resource: file:///b (text/markdown)]"},
		},
		{
			name: "embedded_blob_resource_renders",
			result: &mcp.CallToolResult{Content: []mcp.Content{
				mcp.EmbeddedResource{Resource: mcp.BlobResourceContents{URI: "file:///c", MIMEType: "application/pdf"}},
			}},
			want: []string{"[resource: file:///c (application/pdf)]"},
		},
		{
			name:    "is_error_maps_through",
			result:  &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.TextContent{Text: "boom"}}},
			want:    []string{"boom"},
			isError: true,
		},
		{
			name:   "structured_content_fallback",
			result: &mcp.CallToolResult{RawStructuredContent: json.RawMessage(`{"a":1}`)},
			want:   []string{`{"a":1}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mapCallResult(tc.result)
			assert.Equal(t, tc.isError, res.IsError)
			assert.Equal(t, tc.want, res.Content)

			var blocks []string
			for _, b := range res.toBlocks() {
				blocks = append(blocks, b.(llm.TextBlock).Text)
			}
			assert.Equal(t, tc.want, blocks)
		})
	}
}

func TestRefTextEmbeddedResource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   mcp.Content
		want string
	}{
		{
			name: "embedded_without_mime",
			in:   mcp.EmbeddedResource{Resource: mcp.TextResourceContents{URI: "file:///d"}},
			want: "[resource: file:///d]",
		},
		{
			name: "nil_resource_contents",
			in:   mcp.EmbeddedResource{},
			want: "[content of unrecognized type]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, refText(tc.in))
		})
	}
}
