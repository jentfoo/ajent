package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/img"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imagePNG returns a small valid png's bytes.
func imagePNG(t *testing.T) []byte {
	t.Helper()

	m := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			m.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	return b.Bytes()
}

// Serial: the cases flip the package-wide images toggle.
func TestFitImage(t *testing.T) {
	t.Run("small png passes through", func(t *testing.T) {
		blocks, err := FitImage(imagePNG(t))
		require.NoError(t, err)
		require.Len(t, blocks, 1)
		img, ok := blocks[0].(llm.ImageBlock)
		require.True(t, ok)
		assert.Equal(t, "image/png", img.MediaType)
	})

	t.Run("disabled yields placeholder", func(t *testing.T) {
		SetImagesEnabled(false)
		t.Cleanup(func() { SetImagesEnabled(true) })
		blocks, err := FitImage(imagePNG(t))
		require.NoError(t, err)
		require.Len(t, blocks, 1)
		assert.Equal(t, imagesDisabledText, blocks[0].(llm.TextBlock).Text)
	})

	t.Run("garbage is refused", func(t *testing.T) {
		_, err := FitImage([]byte("not an image"))
		require.Error(t, err)
		assert.Contains(t, FitImageError(err), "unsupported")
	})

	t.Run("irreducible message", func(t *testing.T) {
		assert.Contains(t, FitImageError(img.ErrIrreducible), "size limit")
	})
}

// Serial: the cases flip the package-wide images toggle.
func TestNormalizeImageBlocks(t *testing.T) {
	t.Run("text only untouched", func(t *testing.T) {
		in := llm.BlockList{llm.TextBlock{Text: "hello"}}
		assert.Equal(t, in, NormalizeImageBlocks(in))
	})

	t.Run("valid image unchanged", func(t *testing.T) {
		in := llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: imagePNG(t)}}
		assert.Equal(t, in, NormalizeImageBlocks(in))
	})

	t.Run("oversize image is downscaled", func(t *testing.T) {
		m := image.NewRGBA(image.Rect(0, 0, 2600, 2000))
		var b bytes.Buffer
		require.NoError(t, png.Encode(&b, m))
		out := NormalizeImageBlocks(llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: b.Bytes()}})
		require.NotEmpty(t, out)
		img, ok := out[0].(llm.ImageBlock)
		require.True(t, ok)
		assert.LessOrEqual(t, len(img.Data), len(b.Bytes()))
		// the sizing note rides along when dimensions changed
		require.Len(t, out, 2)
		assert.Contains(t, out[1].(llm.TextBlock).Text, "original 2600x2000")
	})

	t.Run("disabled replaces with placeholder", func(t *testing.T) {
		SetImagesEnabled(false)
		t.Cleanup(func() { SetImagesEnabled(true) })
		out := NormalizeImageBlocks(llm.BlockList{
			llm.TextBlock{Text: "before"},
			llm.ImageBlock{MediaType: "image/png", Data: imagePNG(t)},
		})
		require.Len(t, out, 2)
		assert.Equal(t, "before", out[0].(llm.TextBlock).Text)
		assert.Equal(t, imagesDisabledText, out[1].(llm.TextBlock).Text)
	})

	t.Run("corrupt image keeps block with note", func(t *testing.T) {
		in := llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: []byte{0, 1, 2}}}
		out := NormalizeImageBlocks(in)
		require.Len(t, out, 2)
		assert.Equal(t, in[0], out[0])
		assert.Contains(t, out[1].(llm.TextBlock).Text, "kept unnormalized")
	})

	t.Run("huge undecodable image is dropped", func(t *testing.T) {
		// png magic over megabytes of nothing: no decoder accepts it, so the
		// bytes must not ride history forever
		blob := append([]byte{0x89, 'P', 'N', 'G'}, make([]byte, 7<<20)...)
		out := NormalizeImageBlocks(llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: blob}})
		require.Len(t, out, 1)
		note, ok := out[0].(llm.TextBlock)
		require.True(t, ok)
		assert.Contains(t, note.Text, "dropped")
	})
}

// imgTool is a raw tool returning fixed blocks, unguarded by the registry.
type imgTool struct{ blocks llm.BlockList }

func (imgTool) Name() string                { return "mcp_shot" }
func (imgTool) Label(agent.ToolCall) string { return "mcp_shot: ..." }
func (imgTool) Description() string         { return "test tool" }
func (imgTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: "mcp_shot"} }
func (imgTool) Mode() agent.ExecutionMode   { return agent.ModeParallel }
func (imgTool) Close()                      {}

func (t imgTool) Execute(context.Context, agent.ToolCall, agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{Content: t.blocks}, nil
}

func TestNormalizeImages(t *testing.T) {
	t.Parallel()

	t.Run("oversize result image is fitted", func(t *testing.T) {
		m := image.NewRGBA(image.Rect(0, 0, 2600, 2000))
		var b bytes.Buffer
		require.NoError(t, png.Encode(&b, m))
		wrapped := NormalizeImages(imgTool{blocks: llm.BlockList{
			llm.ImageBlock{MediaType: "image/png", Data: b.Bytes()},
		}})

		res, err := wrapped.Execute(t.Context(), agent.ToolCall{ID: "c", Name: "mcp_shot"}, agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		fitted, ok := res.Content[0].(llm.ImageBlock)
		require.True(t, ok)
		assert.Less(t, len(fitted.Data), len(b.Bytes()))
	})

	t.Run("text result is untouched", func(t *testing.T) {
		in := llm.BlockList{llm.TextBlock{Text: "hello"}}
		wrapped := NormalizeImages(imgTool{blocks: in})

		res, err := wrapped.Execute(t.Context(), agent.ToolCall{ID: "c", Name: "mcp_shot"}, agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		assert.Equal(t, in, res.Content)
	})

	t.Run("metadata delegates", func(t *testing.T) {
		wrapped := NormalizeImages(imgTool{})
		assert.Equal(t, "mcp_shot", wrapped.Name())
		assert.Equal(t, "test tool", wrapped.Description())
		assert.Equal(t, agent.ModeParallel, wrapped.Mode())
	})
}
