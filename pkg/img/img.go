// Package img fits user-supplied images to what vision providers accept: it
// decodes png, jpeg, gif, webp, bmp and tiff, downscales to the inline limits
// rather than rejecting, and re-encodes to png or jpeg. It is the only place
// the x/image dependencies are imported, so callers deal in bytes and results.
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
	Frames    int    // frames an animated source showed or announced, 1 when unknown
}

// Resized reports whether the pixel dimensions changed from the source.
func (r Result) Resized() bool { return r.Width != r.SrcWidth || r.Height != r.SrcHeight }

// tooManyPixels reports whether a header claim of w by h pixels exceeds the
// decode budget. Both must be positive: their product can overflow int, so the
// budget is compared by division.
func tooManyPixels(w, h int) bool { return w > maxDecodePixels/h }

// Prepare fits data to the inline limits: at most MaxPixels per axis and a
// base64 encoding no longer than MaxBase64Bytes. An image already within both
// passes through unchanged, everything else is downscaled and re-encoded until
// it fits, with decode-only formats such as bmp or tiff always converted. EXIF
// orientation is applied whenever an image is re-encoded, so a camera shot that
// needed fitting comes out upright.
//
// Animation never survives: every animated source is reduced to its first
// frame before the fit decision, whatever the limits say, because the inline
// endpoints document still images only (OpenAI asks for non-animated gif;
// Anthropic renders first frames; Google lists no gif at all). The loss is
// disclosed through Flattened and Frames.
//
// Returns ErrUnsupported when no decoder accepts the bytes, ErrTooLarge when
// the header claims more pixels than may ever be decoded however small the
// file, and ErrIrreducible when nothing it can encode fits both limits.
func Prepare(data []byte) (Result, error) {
	mediaType, cfg, ok := sniff(data)
	if !ok || cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, ErrUnsupported
	}
	if tooManyPixels(cfg.Width, cfg.Height) {
		return Result{}, ErrTooLarge
	}
	// animation never rides through: providers document still images only,
	// so every animated source is reduced to frame one before the fit check
	var frames int // frames in the source, 0 for a still
	var frame []byte
	switch mediaType {
	case typePNG:
		if n, ok := apngFrames(data); ok {
			frames = n
		}
	case typeGIF:
		// an unwalkable tail may hide frames past the ones counted
		if n, complete := scanGIF(data); n >= 2 || (!complete && n > 0) {
			frames = n
		}
	case typeWebP:
		frames, frame = webpAnimation(data)
	}
	if frame != nil { // leading frame rides on as a plain still
		mt, c2, ok := sniff(frame)
		if !ok || c2.Width <= 0 || c2.Height <= 0 {
			return Result{}, ErrUnsupported
		}
		data, mediaType, cfg = frame, mt, c2
	}
	flattened := frames > 0

	if !flattened && cfg.Width <= MaxPixels && cfg.Height <= MaxPixels && base64Fits(len(data)) {
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
		SrcWidth: sw, SrcHeight: sh, Converted: mt != mediaType,
		Flattened: flattened, Frames: frames,
	}, nil
}

// ToPNG converts data to png bytes, ok false when decoding or encoding fails.
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

	// walk the segment table until the APP1 EXIF block, but other APP1 payloads
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
	// each IFD entry is a fixed twelve bytes; a crafted or truncated count can
	// claim more entries than the block holds, so clamp before slicing
	count = min(count, (len(tiff)-(off+2))/12)
	for n := range count {
		e := tiff[off+2+n*12:]
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

// encodeFit returns an encoding of src, whose dimensions are w and h, that fits
// MaxPixels per axis and the base64 size ceiling, along with its media type and
// final dimensions. The largest fitting size wins, png preferred over jpeg.
// A jpeg result has transparency flattened onto white. ok is false when no
// encoding can fit.
func encodeFit(src image.Image, w, h int) (data []byte, mediaType string, fw, fh int, ok bool) {
	clamp := min(1, float64(MaxPixels)/float64(w), float64(MaxPixels)/float64(h))
	for _, f := range shrinkFactors {
		scale := f * clamp
		cw := max(int(float64(w)*scale+0.5), 1)
		ch := max(int(float64(h)*scale+0.5), 1)
		cand := src
		if cw != w || ch != h {
			dst := image.NewNRGBA(image.Rect(0, 0, cw, ch))
			draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
			cand = dst
		}
		if b, err := encodePNG(cand); err == nil && base64Fits(len(b)) {
			return b, typePNG, cw, ch, true
		}
		// jpeg keeps no alpha: composite over white first, so a fallback from
		// png renders transparency as white rather than whatever rgb hid beneath
		cb := cand.Bounds()
		flat := image.NewNRGBA(image.Rect(0, 0, cb.Dx(), cb.Dy()))
		draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), cand, cb.Min, draw.Over)
		for _, q := range jpegQualities {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: q}); err != nil {
				continue
			}
			if b := buf.Bytes(); base64Fits(len(b)) {
				return b, typeJPEG, cw, ch, true
			}
		}
	}
	return nil, "", 0, 0, false
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
