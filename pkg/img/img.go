// Package img fits user-supplied images to what vision providers accept: it
// decodes png, jpeg, gif, webp, bmp and tiff, downscales to the inline limits
// rather than rejecting, and re-encodes to png or jpeg. It is the only place
// the x/image dependencies are imported; callers deal in bytes and results.
// The webp registration mirrors the one in pkg/llm, which reads image headers
// without decoding. Decode, EXIF orientation and resampling use stdlib plus
// golang.org/x/image/draw only.
package img

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"image/png"

	draw "golang.org/x/image/draw"

	_ "golang.org/x/image/bmp" // register bmp/tiff decoding into image.Decode (no stdlib encoders)
	_ "golang.org/x/image/tiff"
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
	src, err := decode(data)
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
	src, err := decode(data)
	if err != nil {
		return nil, false
	}
	b, err := encodePNG(src)
	if err != nil {
		return nil, false
	}
	return b, true
}

// decode reads one frame from data, applying the JPEG EXIF orientation tag so
// a camera shot that needed fitting comes out upright.
func decode(data []byte) (image.Image, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if o := jpegOrientation(data); o > orientationNormal {
		return fixOrientation(src, o), nil
	}
	return src, nil
}

// EXIF orientation tags, per the JPEG standard.
const (
	orientationNormal     = 1 // no transform (also used when unspecified)
	orientationFlipH      = 2
	orientationRotate180  = 3
	orientationFlipV      = 4
	orientationTranspose  = 5
	orientationRotate270  = 6
	orientationTransverse = 7
	orientationRotate90   = 8
)

// jpegOrientation reads the EXIF orientation tag from JPEG bytes, or 0 when
// data is not a jpeg carrying one.
func jpegOrientation(data []byte) int {
	const (
		markerSOI  = 0xffd8
		markerAPP1 = 0xffe1
	)
	if len(data) < 4 || binary.BigEndian.Uint16(data[:2]) != markerSOI {
		return 0
	}

	// walk the segment table until the APP1 EXIF block; other APP1 payloads
	// (XMP and friends) keep the walk going
	i := 2 // past SOI
	for i+4 <= len(data) {
		marker := binary.BigEndian.Uint16(data[i : i+2])
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if marker>>8 != 0xff || size < 2 {
			return 0
		}
		i += 4 // past the length field, onto this segment's payload
		payload := size - 2
		if i+payload > len(data) {
			return 0
		}
		if marker == markerAPP1 && payload >= 4 && string(data[i:i+4]) == "Exif" {
			break
		}
		i += payload
	}

	o, ok := exifOrientation(data[i:])
	if !ok || o < orientationNormal || o > orientationRotate90 {
		return 0
	}
	return o
}

// exifOrientation reads the EXIF orientation tag from an "Exif\0\0..." APP1
// payload, ok false when the block is absent or malformed. The IFD offset field
// is relative to the TIFF header that follows the Exif signature.
func exifOrientation(p []byte) (int, bool) {
	const orientationTag = 0x0112
	if len(p) < 6 || string(p[:4]) != "Exif" {
		return 0, false
	}
	tiff := p[6:] // TIFF: byte-order flag, magic, then the IFD offset field
	if len(tiff) < 8 {
		return 0, false
	}
	var bo binary.ByteOrder
	switch be := binary.BigEndian.Uint16(tiff); be {
	case 0x4d4d: // MM big-endian
		bo = binary.BigEndian
	case 0x4949: // II little-endian
		bo = binary.LittleEndian
	default:
		return 0, false
	}
	off := int(bo.Uint32(tiff[4:8]))
	if off <= 6 || len(tiff) < off+2 {
		return 0, false // IFD count would sit outside the block
	}

	count := int(bo.Uint16(tiff[off : off+2]))
	for n := range count { // each IFD entry is a fixed twelve bytes
		e := tiff[off+2+n*12:]
		if len(e) < 10 {
			return 0, false
		}
		if bo.Uint16(e[:2]) != orientationTag {
			continue
		}
		v := int(bo.Uint16(e[8:10])) // tag id, type+count, then the value field
		return v, true
	}
	return 0, false
}

// fixOrientation applies the EXIF transform o to src, returning an NRGBA.
func fixOrientation(src image.Image, o int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || o == orientationNormal {
		return toNRGBA(src)
	}

	dw, dh := w, h
	switch o {
	case orientationTranspose, orientationRotate270, orientationTransverse, orientationRotate90:
		dw, dh = h, w
	}
	srcN := toNRGBA(src)
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			sx, sy := orientSrc(o, x, y, w, h)
			si, di := srcN.PixOffset(sx, sy), dst.PixOffset(x, y)
			copy(dst.Pix[di:di+4], srcN.Pix[si:si+4])
		}
	}
	return dst
}

// orientSrc maps a destination pixel to its source for the given orientation.
func orientSrc(o, x, y, w, h int) (int, int) {
	switch o {
	case orientationFlipH:
		return w - 1 - x, y
	case orientationRotate180:
		return w - 1 - x, h - 1 - y
	case orientationFlipV:
		return x, h - 1 - y
	case orientationTranspose: // out(x,y)=in(y,x)
		return y, x
	case orientationRotate270: // out(x,y)=in(row=h-1-x,col=y)
		return y, h - 1 - x
	case orientationTransverse: // out(x,y)=in(h-1-x,w-1-y)
		return w - 1 - y, h - 1 - x
	default: // orientationRotate90: out(x,y)=in(row=x,col=w-1-y)
		return w - 1 - y, x
	}
}

// toNRGBA converts any image into a plain NRGBA buffer.
func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}
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
			cand = resize(src, cw, ch)
		}
		if b, err := encodePNG(cand); err == nil && base64Fits(len(b)) {
			return b, typePNG, cw, ch, true
		}
		// jpeg keeps no alpha: composite over white first, so a fallback from
		// png renders transparency as white rather than whatever rgb hid beneath
		flat := flattenWhite(cand)
		for _, q := range jpegQualities {
			if b, err := encodeJPEG(flat, q); err == nil && base64Fits(len(b)) {
				return b, typeJPEG, cw, ch, true
			}
		}
	}
	return nil, "", 0, 0, false
}

// resize downscales src to w x h with a high-quality cubic filter.
func resize(src image.Image, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// flattenWhite composites src over an opaque white canvas, dropping alpha the
// way jpeg requires while rendering transparency as white.
func flattenWhite(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
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
