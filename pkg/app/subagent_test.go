package app

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

// oversizedTool returns one image block far past the pixel ceiling.
type oversizedTool struct{ data []byte }

func (oversizedTool) Name() string                { return "mcp_shot" }
func (oversizedTool) Label(agent.ToolCall) string { return "mcp_shot: ..." }
func (oversizedTool) Description() string         { return "test tool" }
func (oversizedTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: "mcp_shot"} }
func (oversizedTool) Mode() agent.ExecutionMode   { return agent.ModeParallel }
func (oversizedTool) Close()                      {}

func (t oversizedTool) Execute(context.Context, agent.ToolCall, agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{Content: llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: t.data}}}, nil
}

func oversizedPNG(t *testing.T) []byte {
	t.Helper()

	// noisy and just past the per-axis ceiling: noise keeps the encoding from
	// compressing, so the fitted re-encode is strictly smaller, while the small
	// pixel count keeps the fit fast under -race
	const w, h = 2400, 60
	rng := rand.New(rand.NewSource(1))
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range m.Pix {
		if i%4 != 3 {
			m.Pix[i] = byte(rng.Intn(256))
		}
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	return b.Bytes()
}

// A child runs raw registry tools with no guard chain, so the source must wrap
// every one: an unfitted oversized image would ride the child's whole history.
func TestChildToolSource(t *testing.T) {
	t.Parallel()

	png := oversizedPNG(t)
	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir()})
	require.NoError(t, err)
	reg.RegisterFrom("mcp", oversizedTool{data: png}, true)
	reg.MarkReadOnly([]string{"mcp_shot"})

	src := childToolSource{reg: reg}

	// every registered tool survives under its own name, now wrapped
	assert.Equal(t, namesOf(reg.All()), namesOf(src.All()))
	assert.True(t, src.ReadOnly("mcp_shot"))
	assert.False(t, src.ReadOnly("read"))

	all := src.All()
	wrapped := all[len(all)-1]
	assert.Equal(t, "mcp_shot", wrapped.Name())

	res, err := wrapped.Execute(t.Context(), agent.ToolCall{ID: "c", Name: "mcp_shot"},
		agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)
	fitted, ok := res.Content[0].(llm.ImageBlock)
	require.True(t, ok)
	assert.Less(t, len(fitted.Data), len(png))
}

// namesOf returns the tool names in order.
func namesOf(ts []agent.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
