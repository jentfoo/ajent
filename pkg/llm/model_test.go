package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModelAccepts(t *testing.T) {
	t.Parallel()

	m := Model{Input: []Modality{ModalityText, ModalityImage}}
	assert.True(t, m.Accepts(ModalityImage))
	assert.False(t, Model{Input: []Modality{ModalityText}}.Accepts(ModalityImage))
	assert.False(t, Model{}.Accepts(ModalityText))
}
