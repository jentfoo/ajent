package img

import (
	"bytes"
	"image"
)

// Media type constants reported by Sniff. png, jpeg, gif and webp can be
// sent inline as-is; bmp and tiff are decode-only inputs that Prepare converts.
const (
	TypePNG  = "image/png"
	TypeJPEG = "image/jpeg"
	TypeGIF  = "image/gif"
	TypeWebP = "image/webp"
	TypeBMP  = "image/bmp"
	TypeTIFF = "image/tiff"
)

// sniffTypes maps the format names image.DecodeConfig reports onto media types.
var sniffTypes = map[string]string{
	"png":  TypePNG,
	"jpeg": TypeJPEG,
	"gif":  TypeGIF,
	"webp": TypeWebP,
	"bmp":  TypeBMP,
	"tiff": TypeTIFF,
}

// Sniff reports the media type of an image from its magic bytes, ok false for
// anything unregistered. Header-only: nothing is decoded.
func Sniff(data []byte) (mediaType string, ok bool) {
	mt, _, ok := sniff(data)
	return mt, ok
}

// sniff reads one header for both the media type and the dimensions.
func sniff(data []byte) (mediaType string, cfg image.Config, ok bool) {
	c, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", image.Config{}, false
	}
	mt, ok := sniffTypes[format]
	if !ok {
		return "", image.Config{}, false
	}
	return mt, c, true
}
