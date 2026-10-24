package img

import "encoding/binary"

// Animation detection and frame extraction for Prepare, all header-only.

const (
	gifExtension       = 0x21 // label byte, then length-prefixed sub-blocks
	gifImageDescriptor = 0x2c // one frame of pixel data
	gifTrailer         = 0x3b // end of file

	// webpAnimationBit is the VP8X flag marking an animated webp.
	webpAnimationBit = 1 << 1
)

// apngFrames returns the frame count an acTL chunk announces, ok false for a
// still png without one. Stops at the first frame data.
func apngFrames(data []byte) (frames int, ok bool) {
	for i := 8; i+8 <= len(data); {
		n := binary.BigEndian.Uint32(data[i : i+4])
		switch string(data[i+4 : i+8]) {
		case "acTL": // num_frames, then num_plays
			// a declared count needs its bytes present: a truncated payload reads
			// as animated with an unknown count, never past the end of the file
			if n >= 4 && i+12 <= len(data) {
				return max(int(binary.BigEndian.Uint32(data[i+8:i+12])), 1), true
			}
			return 1, true
		case "IDAT", "IEND": // animation controls precede the first frame
			return 0, false
		}
		step := 12 + int(n)
		if step < 12 { // length overflowed int: unwalkable
			return 0, false
		}
		i += step
	}
	return 0, false
}

// scanGIF reports how many frames a gif announces by counting image
// descriptors, ok false when the walk cannot reach the trailer. A hand walk:
// no stdlib call counts frames without decoding every one. An unwalkable tail
// may hide more frames than counted.
func scanGIF(data []byte) (frames int, ok bool) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return 0, false
	}
	i := 13                 // past header and logical screen descriptor
	if data[10]&0x80 != 0 { // global color table follows the descriptor
		i += gifColorTableBytes(data[10])
		if i > len(data) {
			return 0, false
		}
	}
	for i < len(data) {
		switch data[i] {
		case gifTrailer:
			return frames, true
		case gifImageDescriptor:
			frames++
			if i+10 > len(data) { // descriptor plus its packed field
				return frames, false
			}
			i += 10
			if data[i-1]&0x80 != 0 { // local color table follows the descriptor
				i += gifColorTableBytes(data[i-1])
				if i > len(data) {
					return frames, false
				}
			}
			var good bool
			if i, good = skipGIFSubBlocks(data, i+1); !good { // +1: LZW min code size byte
				return frames, false
			}
		case gifExtension:
			var good bool
			if i, good = skipGIFSubBlocks(data, i+2); !good { // introducer and label
				return frames, false
			}
		default:
			return frames, false
		}
	}
	return frames, false // ended without a trailer: truncated
}

// skipGIFSubBlocks returns the offset just past a chain of length-prefixed
// sub-blocks and its zero terminator, ok false when the data ends mid-chain.
func skipGIFSubBlocks(data []byte, i int) (int, bool) {
	for {
		if i >= len(data) {
			return 0, false
		}
		n := int(data[i])
		i++
		if n == 0 {
			return i, true
		}
		i += n
		if i > len(data) {
			return 0, false
		}
	}
}

// gifColorTableBytes is the byte size of the color table a packed field announces.
func gifColorTableBytes(packed byte) int {
	return 3 * (1 << ((packed & 0x07) + 1))
}

// webpAnimation reports the frame count of an animated webp and its leading
// frame as plain still bytes, nil when none is decodable. The animation flag
// alone counts one frame; alpha in ALPH chunks is dropped.
func webpAnimation(data []byte) (frames int, still []byte) {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" ||
		string(data[12:16]) != "VP8X" || data[20]&webpAnimationBit == 0 {
		return 0, nil
	}
	for i := 12; i+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
		if size < 0 { // length overflowed int: unwalkable
			break
		}
		if string(data[i:i+4]) == "ANMF" && i+8+size <= len(data) {
			frames++
			if still == nil {
				still = webpFrameStill(data[i+8 : i+8+size])
			}
		}
		i += 8 + size + size&1 // chunks pad to even offsets
	}
	return max(frames, 1), still
}

// webpFrameStill rewraps the leading image bitstream of one ANMF payload as a
// single-frame webp.
func webpFrameStill(anmf []byte) []byte {
	for i := 16; i+8 <= len(anmf); { // past the frame header: rect, duration, flags
		size := int(binary.LittleEndian.Uint32(anmf[i+4 : i+8]))
		if size < 0 {
			return nil
		}
		switch string(anmf[i : i+4]) {
		case "VP8 ", "VP8L":
			n := min(size, len(anmf)-i-8)
			chunk := anmf[i : i+8+n]
			out := binary.LittleEndian.AppendUint32(nil, 0x46464952) // RIFF
			out = binary.LittleEndian.AppendUint32(out, 0)           // size, patched below
			out = append(out, "WEBP"...)
			out = append(out, chunk...)
			if len(chunk)&1 == 1 { // chunks pad to even sizes
				out = append(out, 0)
			}
			binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
			return out
		}
		i += 8 + size + size&1
	}
	return nil
}
