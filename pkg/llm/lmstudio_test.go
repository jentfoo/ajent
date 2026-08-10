package llm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLMStudioModels(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "lmstudio", "models.json"))
	require.NoError(t, err)

	got, err := parseLMStudioModels(data)
	require.NoError(t, err)

	t.Run("skips_embedding_models", func(t *testing.T) {
		require.Len(t, got, 2)
		for _, m := range got {
			assert.NotEqual(t, "nomic-embed-text", m.ID)
		}
	})
	t.Run("loaded_context_length_wins", func(t *testing.T) {
		// the user loaded it smaller than its maximum, and that is what fits
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 65536, *got[0].ContextWindow)
	})
	t.Run("max_context_length_when_not_loaded", func(t *testing.T) {
		require.NotNil(t, got[1].ContextWindow)
		assert.Equal(t, 400000, *got[1].ContextWindow)
	})
	t.Run("vision_capability_adds_the_image_modality", func(t *testing.T) {
		assert.Equal(t, []Modality{ModalityText, ModalityImage}, got[0].Input)
		assert.Equal(t, []Modality{ModalityText}, got[1].Input)
	})
	t.Run("tool_use_capability_recorded", func(t *testing.T) {
		require.NotNil(t, got[0].Compat)
		require.NotNil(t, got[0].Compat.SupportsToolChoice)
		assert.True(t, *got[0].Compat.SupportsToolChoice)
	})
	t.Run("malformed_body_errors", func(t *testing.T) {
		_, err := parseLMStudioModels([]byte("not json"))
		assert.Error(t, err)
	})
	t.Run("empty_list", func(t *testing.T) {
		out, err := parseLMStudioModels([]byte(`{"data":[]}`))
		require.NoError(t, err)
		assert.Empty(t, out)
	})
}

func TestLMStudioDiscovery(t *testing.T) {
	t.Parallel()

	srv, req := jsonServer(t, "lmstudio/models.json")
	c, _, _ := testClient(t, srv.URL)

	got, err := discoverProvider(t.Context(), c, "/api/v0/models", parseLMStudioModels, CacheEntry{}, testNow)
	require.NoError(t, err)

	assert.Equal(t, "/api/v0/models", req.Path)
	require.Len(t, got.Models, 2)
	assert.Equal(t, "qwen3.6-27b-mtp", got.Models[0].ID)
}
