package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

// testImage is a tiny payload; block transport, not encoding, is under test.
var testImage = llm.ImageBlock{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}

func TestInputBlocksRideToRequest(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("seen")}}}
	// image-capable: the agent hands the provider a prepared request, so a
	// caps-less model would see the image already downgraded before llm
	st := &State{Model: llm.Model{ID: "test", Caps: llm.Capabilities{Images: true}},
		Reasoning: llm.ReasoningConfig{}}
	a := newTestAgent(st, p, nil)

	require.NoError(t, a.Prompt(t.Context(), Input{
		Text:   "look",
		Blocks: llm.BlockList{testImage},
	}))

	reqs := p.Requests()
	require.Len(t, reqs, 1)
	var last *llm.Message
	for i := range reqs[0].Messages {
		m := &reqs[0].Messages[i]
		if m.Role == llm.RoleUser {
			last = m
		}
	}
	require.NotNil(t, last)
	require.Len(t, last.Content, 2) // text then the image, in order
	tb, ok := last.Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "look", tb.Text)
	ib, ok := last.Content[1].(llm.ImageBlock)
	require.True(t, ok)
	assert.Equal(t, testImage.MediaType, ib.MediaType)
	assert.Equal(t, testImage.Data, ib.Data)
}

func TestInputBlocksOnlyTurn(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("seen")}}}
	// image-capable so the prepared request still carries the block; see above
	a := newTestAgent(&State{Model: llm.Model{ID: "test", Caps: llm.Capabilities{Images: true}},
		Reasoning: llm.ReasoningConfig{}}, p, nil)

	// a blocks-only input is still a turn: the payload lands without text
	require.NoError(t, a.Prompt(t.Context(), Input{Blocks: llm.BlockList{testImage}}))
	reqs := p.Requests()
	require.Len(t, reqs, 1)
	found := false
	for _, m := range reqs[0].Messages {
		for _, b := range m.Content {
			if ib, ok := b.(llm.ImageBlock); ok && string(ib.Data) == string(testImage.Data) {
				found = true
			}
		}
	}
	assert.True(t, found)
}
