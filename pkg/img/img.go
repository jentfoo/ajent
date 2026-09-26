// Package img fits user-supplied images to what vision providers accept: it
// decodes png, jpeg, gif, webp, bmp and tiff, downscales to the inline limits
// rather than rejecting, and re-encodes to png or jpeg. It is the only place
// the imaging and webp dependencies are imported; callers deal in bytes and
// results. The webp registration mirrors the one in pkg/llm, which reads
// image headers without decoding.
package img

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp" // registers webp decoding into image.Decode
)

// MaxPixels is the per-axis pixel ceiling for an inline image.
const MaxPixels = 2000

// MaxBase64Bytes is the encoded-size ceiling for an inline image.
const MaxBase64Bytes = 4_500_000

// maxDecodePixels bounds the full decode of an oversized image: a full frame
// allocates width x height up front, so a header claiming vast dimensions must
// be refused before the memory is promised.
const maxDecodePixels = 100_000_000

// jpegQualities are the re-encode qualities the shrink loop's jpeg arm tries
// at each size, best first: quality loss is cheaper than resolution loss.
var jpegQualities = []int{80, 65, 50}

// shrinkFactors are absolute fractions of the source size, tried in order. Each
// rung resizes the source once, so no rung resamples a previous rung's output.
var shrinkFactors = []float64{1, 0.75, 0.5, 0.375, 0.25, 0.125}

// Errors reported by Prepare.
var (
	// ErrUnsupported means the bytes are not a decodable image.
	ErrUnsupported = errors.New("img: not a decodable image (png, jpeg, gif, webp, bmp, tiff accepted)")
	// ErrTooLarge means the header claims more pixels than decoding may
	// allocate for, however small the file is.
	ErrTooLarge = errors.New("img: image dimensions exceed the decodable limit")
	// ErrIrreducible means even a 1x1 re-encode would not fit the ceiling.
	ErrIrreducible = errors.New("img: image cannot be brought under the inline size limit")
)

// Result is one fitted image and how it compares with its source.
type Result struct {
	Data      []byte // encoded image bytes, in MediaType
	MediaType string // image/png, image/jpeg, image/gif or image/webp
	Width     int    // final pixel width
	Height    int    // final pixel height
	SrcWidth  int    // source pixel width, after EXIF orientation
	SrcHeight int    // source pixel height, after EXIF orientation
	Converted bool   // the media type differs from the source format
	Flattened bool   // the source was animated and one frame was kept
}

// Resized reports whether the pixel dimensions changed from the source.
func (r Result) Resized() bool { return r.Width != r.SrcWidth || r.Height != r.SrcHeight }

// Prepare fits data to the inline limits: at most MaxPixels per axis and a
// base64 encoding no longer than MaxBase64Bytes. An image already within both
// passes through unchanged; everything else is downscaled and re-encoded until
// it fits, or ErrIrreducible when even the last rung would not. EXIF
// orientation is applied whenever an image is re-encoded, so a camera shot
// that needed fitting comes out upright. An animated png loses its animation
// to the re-encode, keeping the first frame.
func Prepare(data []byte) (Result, error) {
	mediaType, cfg, ok := sniff(data)
	if !ok || cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, ErrUnsupported
	}
	if cfg.Width*cfg.Height > maxDecodePixels {
		return Result{}, ErrTooLarge
	}
	animated := animatedPNG(data, mediaType)
	if !animated && withinLimits(cfg.Width, cfg.Height, len(data)) {
		switch mediaType {
		case typePNG, typeJPEG, typeGIF, typeWebP: // provider-accepted as-is
			return Result{Data: data, MediaType: mediaType,
				Width: cfg.Width, Height: cfg.Height, SrcWidth: cfg.Width, SrcHeight: cfg.Height,
			}, nil
		}
	}

	// decode even a sendable format that busts a limit: the header dimensions
	// ignore EXIF rotation, so the oriented bounds are the honest source size
	src, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return Result{}, ErrUnsupported
	}
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw <= 0 || sh <= 0 {
		return Result{}, ErrUnsupported
	}
	out, mt, w, h, ok := encodeFit(src, sw, sh)
	if !ok {
		return Result{}, ErrIrreducible
	}
	return Result{Data: out, MediaType: mt, Width: w, Height: h,
		SrcWidth: sw, SrcHeight: sh, Converted: mt != mediaType, Flattened: animated,
	}, nil
}

// ToPNG converts data to png bytes, ok false when no decoder accepts it.
// EXIF orientation is applied.
func ToPNG(data []byte) ([]byte, bool) {
	src, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, false
	}
	b, err := encodePNG(src)
	if err != nil {
		return nil, false
	}
	return b, true
}

// withinLimits reports whether w x h with n raw bytes fits the ceilings.
func withinLimits(w, h, n int) bool {
	return w <= MaxPixels && h <= MaxPixels && base64Fits(n)
}

// animatedPNG reports whether data is a png carrying an animation control
// chunk. Left alone such a file would ride to the provider verbatim and some
// reject the whole request over it, so it is re-encoded to its first frame.
func animatedPNG(data []byte, mediaType string) bool {
	if mediaType != typePNG || len(data) < 8 {
		return false
	}
	for i := 8; i+8 <= len(data); {
		n := binary.BigEndian.Uint32(data[i : i+4])
		typ := string(data[i+4 : i+8])
		switch typ {
		case "acTL":
			return true
		case "IDAT": // animation controls precede the first frame
			return false
		case "IEND":
			return false
		}
		i += 12 + int(n) // length, type, data, crc
	}
	return false
}

// encodeFit searches encodings of src for the first within the size ceiling,
// starting from the source fitted proportionally under the pixel ceiling and
// stepping down the shrink ladder toward 1x1. Each size tries png, then jpeg
// at descending qualities. ok is false only when even the last rung fails.
func encodeFit(src image.Image, w, h int) (data []byte, mediaType string, fw, fh int, ok bool) {
	clamp := min(1, float64(MaxPixels)/float64(w), float64(MaxPixels)/float64(h))
	for _, f := range shrinkFactors {
		cw, ch := scaleTo(w, h, f*clamp)
		cand := src
		if cw != w || ch != h {
			cand = imaging.Resize(src, cw, ch, imaging.Lanczos)
		}
		if b, err := encodePNG(cand); err == nil && base64Fits(len(b)) {
			return b, typePNG, cw, ch, true
		}
		// jpeg keeps no alpha: composite over white first, so a fallback from
		// png renders transparency as white rather than whatever rgb hid beneath
		flat := imaging.Overlay(imaging.New(cw, ch, color.White), cand, image.Point{}, 1)
		for _, q := range jpegQualities {
			if b, err := encodeJPEG(flat, q); err == nil && base64Fits(len(b)) {
				return b, typeJPEG, cw, ch, true
			}
		}
	}
	return nil, "", 0, 0, false
}

// scaleTo rounds w x h by f, never below 1.
func scaleTo(w, h int, f float64) (int, int) {
	return max(int(float64(w)*f+0.5), 1), max(int(float64(h)*f+0.5), 1)
}

// base64Fits reports whether n bytes fit the size ceiling once base64 encoded.
func base64Fits(n int) bool { return base64.StdEncoding.EncodedLen(n) <= MaxBase64Bytes }

// encodePNG encodes src as png.
func encodePNG(src image.Image) ([]byte, error) {
	var b bytes.Buffer
	if err := png.Encode(&b, src); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// encodeJPEG encodes src as jpeg at the given quality.
func encodeJPEG(src image.Image, q int) ([]byte, error) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, src, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
