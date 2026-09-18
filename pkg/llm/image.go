package llm

import (
	"bytes"
	"image"
	_ "image/gif"  // registered for header dimension reads
	_ "image/jpeg" // registered for header dimension reads
	_ "image/png"  // registered for header dimension reads
	"slices"
	"strconv"

	"github.com/jentfoo/ajent/pkg/strutil"
	_ "golang.org/x/image/webp" // registered here so ImageDims never depends on who else linked a decoder
)

// ImageDims reads pixel dimensions from an image header, nothing decoded
// beyond it. ok is false for unregistered or malformed formats.
func ImageDims(data []byte) (w, h int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// HasImageBlocks reports whether any block is an image.
func HasImageBlocks(blocks BlockList) bool {
	return slices.ContainsFunc(blocks, func(b Block) bool {
		_, ok := b.(ImageBlock)
		return ok
	})
}

// ImageBlocks returns the image blocks in blocks, in order.
func ImageBlocks(blocks BlockList) []ImageBlock {
	var out []ImageBlock
	for _, b := range blocks {
		if ib, ok := b.(ImageBlock); ok {
			out = append(out, ib)
		}
	}
	return out
}

// ImageSummary describes an image payload's dimensions and size, e.g.
// "640x480, 12kb", from its header alone. The size alone when the format is
// not registered.
func ImageSummary(data []byte) string {
	s := ""
	if w, h, ok := ImageDims(data); ok {
		s = strconv.Itoa(w) + "x" + strconv.Itoa(h) + ", "
	}
	return s + strutil.HumanSize(int64(len(data)))
}

// ImagePlaceholder is the one text stand-in for an image block, used
// everywhere history must name a picture it does not draw: label when
// non-empty.
func ImagePlaceholder(data []byte, label string) string {
	if label != "" {
		return "[image " + label + " " + ImageSummary(data) + "]"
	}
	return "[image " + ImageSummary(data) + "]"
}
