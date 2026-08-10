package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModelKey(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "lmstudio/qwen/qwen3.6-35b-a3b",
		Model{Provider: "lmstudio", ID: "qwen/qwen3.6-35b-a3b"}.Key())
}

func TestModelAccepts(t *testing.T) {
	t.Parallel()

	m := Model{Input: []Modality{ModalityText, ModalityImage}}
	assert.True(t, m.Accepts(ModalityImage))
	assert.False(t, Model{Input: []Modality{ModalityText}}.Accepts(ModalityImage))
	assert.False(t, Model{}.Accepts(ModalityText))
}

func TestModelDisplay(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Qwen 3.6", Model{ID: "qwen3.6", Name: "Qwen 3.6"}.Display())
	assert.Equal(t, "qwen3.6", Model{ID: "qwen3.6"}.Display())
}
