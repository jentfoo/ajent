package tools

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/img"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// imagesOff gates every image path: on, image blocks are replaced by a short
// text placeholder wherever they would enter a request. Guarded like the rest
// of the package limits because tools run concurrently with configuration.
var (
	imagesMu  sync.RWMutex
	imagesOff bool
)

// SetImagesEnabled flips the block-images setting.
func SetImagesEnabled(on bool) {
	imagesMu.Lock()
	defer imagesMu.Unlock()
	imagesOff = !on
}

// ImagesEnabled reports whether image blocks may reach providers.
func ImagesEnabled() bool {
	imagesMu.RLock()
	defer imagesMu.RUnlock()
	return !imagesOff
}

// imagesDisabledText is what a disabled image becomes wherever it would enter
// a request.
const imagesDisabledText = "(Image reading is disabled.)"

// FitImage turns one user-supplied image payload into model content: an image
// block plus, when the fit changed the image, a coordinate-mapping note. An
// image that cannot fit returns an error; callers surface FitImageError.
func FitImage(data []byte) (llm.BlockList, error) {
	if !ImagesEnabled() {
		return llm.BlockList{llm.TextBlock{Text: imagesDisabledText}}, nil
	}
	res, err := img.Prepare(data)
	if err != nil {
		return nil, err
	}
	blocks := llm.BlockList{llm.ImageBlock{MediaType: res.MediaType, Data: res.Data}}
	if note := res.Note(); note != "" {
		blocks = append(blocks, llm.TextBlock{Text: note})
	}
	return blocks, nil
}

// FitImageError renders the user-facing refusal for a rejected image.
func FitImageError(err error) string {
	switch {
	case errors.Is(err, img.ErrIrreducible):
		return "image rejected: cannot be brought under the inline size limit"
	case errors.Is(err, img.ErrTooLarge):
		return "image rejected: dimensions exceed the decodable limit"
	case errors.Is(err, img.ErrUnsupported):
		return "image rejected: unsupported or corrupt image"
	default:
		return "image rejected: " + err.Error()
	}
}

// NormalizeImageBlocks rewrites every image block inside a tool result, once,
// at its entry into history, so one oversized result cannot poison every later
// request. A block Prepare cannot fit is kept with a note beside it; bytes no
// decoder accepts at several times the ceiling become the note alone.
func NormalizeImageBlocks(content llm.BlockList) llm.BlockList {
	// fast path: results rarely carry images, and this runs on every tool result
	if !llm.HasImageBlocks(content) {
		return content
	}
	var out llm.BlockList
	var changed bool
	for _, b := range content {
		blk, ok := b.(llm.ImageBlock)
		if !ok {
			out = append(out, b)
			continue
		}
		if !ImagesEnabled() {
			out = append(out, llm.TextBlock{Text: imagesDisabledText})
			changed = true
			continue
		}
		nb, note, was := normalizedImage(blk)
		out = append(out, nb)
		if note != "" {
			out = append(out, llm.TextBlock{Text: note})
		}
		changed = changed || was
	}
	if !changed {
		return content
	}
	return out
}

// normalizedImage fits one tool-result image, reporting the block to keep, a
// companion note when anything changed or went wrong, and whether the block is
// not the input verbatim. Failure keeps the original block with the reason
// beside it; success keeps the fitted encoding.
func normalizedImage(blk llm.ImageBlock) (keep llm.Block, note string, changed bool) {
	res, err := img.Prepare(blk.Data)
	if err != nil {
		if over := int64(base64.StdEncoding.EncodedLen(len(blk.Data))); over > 2*img.MaxBase64Bytes {
			// undecodable bytes nobody can read would ride history and every
			// later request forever: the reason replaces them
			return llm.TextBlock{Text: "(image dropped, " + strutil.HumanSize(over) + ": " +
				strings.TrimPrefix(FitImageError(err), "image rejected: ") + ")"}, "", true
		}
		return blk, "(image kept unnormalized: " + FitImageError(err) + ")", true
	}
	if res.MediaType == blk.MediaType && bytes.Equal(res.Data, blk.Data) {
		return blk, res.Note(), res.Note() != ""
	}
	return llm.ImageBlock{MediaType: res.MediaType, Data: res.Data}, res.Note(), true
}
