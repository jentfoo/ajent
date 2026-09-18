package llm

import (
	"bytes"
	"image"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageDims(t *testing.T) {
	t.Parallel()

	m := image.NewRGBA(image.Rect(0, 0, 640, 480))
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))

	t.Run("png", func(t *testing.T) {
		w, h, ok := ImageDims(b.Bytes())
		require.True(t, ok)
		assert.Equal(t, 640, w)
		assert.Equal(t, 480, h)
	})

	t.Run("truncated header fails", func(t *testing.T) {
		_, _, ok := ImageDims(b.Bytes()[:20])
		assert.False(t, ok)
	})

	t.Run("not an image", func(t *testing.T) {
		_, _, ok := ImageDims([]byte("plainly not an image at all"))
		assert.False(t, ok)
	})
}

func TestImageSummary(t *testing.T) {
	t.Parallel()

	m := image.NewRGBA(image.Rect(0, 0, 640, 480))
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))

	assert.Regexp(t, `^640x480, \d+(\.\d+)?[km]?b$`, ImageSummary(b.Bytes()))
	assert.Regexp(t, `^\d+(\.\d+)?[km]?b$`, ImageSummary([]byte("not an image")))
}

func TestImageBlocks(t *testing.T) {
	t.Parallel()

	blocks := BlockList{
		TextBlock{Text: "before"},
		ImageBlock{MediaType: "image/png", Data: []byte{1}},
		TextBlock{Text: "middle"},
		ImageBlock{MediaType: "image/jpeg", Data: []byte{2}},
	}
	imgs := ImageBlocks(blocks)
	require.Len(t, imgs, 2)
	assert.Equal(t, "image/png", imgs[0].MediaType)
	assert.Equal(t, "image/jpeg", imgs[1].MediaType)
	assert.Empty(t, ImageBlocks(BlockList{TextBlock{Text: "none"}}))
}
