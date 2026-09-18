package mcp

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

func TestMapCallResult(t *testing.T) {
	t.Parallel()

	pngData := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake"))

	cases := []struct {
		name    string
		result  *mcp.CallToolResult
		want    []string
		images  int
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
			name:   "image_becomes_block",
			result: &mcp.CallToolResult{Content: []mcp.Content{mcp.ImageContent{MIMEType: "image/png", Data: pngData}}},
			images: 1,
		},
		{
			name:   "image_without_data_is_placeholder",
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

			require.Len(t, res.Blocks, len(tc.want)+tc.images)
			var texts []string
			var images int
			for _, b := range res.Blocks {
				switch v := b.(type) {
				case llm.TextBlock:
					texts = append(texts, v.Text)
				case llm.ImageBlock:
					images++
					assert.Equal(t, "image/png", v.MediaType)
					assert.NotEmpty(t, v.Data)
				}
			}
			assert.Equal(t, tc.images, images)
			assert.Equal(t, tc.want, texts)
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

func TestImageBytes(t *testing.T) {
	t.Parallel()

	t.Run("decodes image payload", func(t *testing.T) {
		t.Parallel()
		b, ok := imageBytes("image/png", base64.StdEncoding.EncodeToString([]byte("abc")))
		assert.True(t, ok)
		assert.Equal(t, []byte("abc"), b)
	})

	t.Run("rejects non image mime", func(t *testing.T) {
		t.Parallel()
		_, ok := imageBytes("text/plain", base64.StdEncoding.EncodeToString([]byte("abc")))
		assert.False(t, ok)
	})

	t.Run("rejects bad base64", func(t *testing.T) {
		t.Parallel()
		_, ok := imageBytes("image/png", "!!!not base64!!!")
		assert.False(t, ok)
	})
}

func TestDisplayOf(t *testing.T) {
	t.Parallel()

	res := Result{Blocks: []llm.Block{
		llm.TextBlock{Text: "chart follows "},
		llm.ImageBlock{MediaType: "image/png", Data: []byte("12345")},
	}}
	assert.Equal(t, "chart follows [image 5b]", displayOf(res))
}
