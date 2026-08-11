package tools

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// fileKind discriminates what a probed path holds.
type fileKind int

const (
	fileText fileKind = iota
	fileBinary
	fileImage
)

// probeFile reads up to the whole of path, reporting its kind and size info. A
// NUL byte in the first 8 kB marks binary; a known image signature marks an image.
func probeFile(path string) (data []byte, info os.FileInfo, kind fileKind, err error) {
	info, err = os.Stat(path)
	if err != nil {
		return nil, nil, fileText, err
	}
	data, err = readAllFile(path)
	if err != nil {
		return nil, nil, fileText, err
	}
	switch detect(data) {
	case fileImage:
		return data, info, fileImage, nil
	case fileBinary:
		return data, info, fileBinary, nil
	default:
		return data, info, fileText, nil
	}
}

const sniffLen = 8 << 10

// detect classifies a buffer as text, binary or image.
func detect(data []byte) (out fileKind) {
	sniff := data
	if len(sniff) > sniffLen {
		sniff = sniff[:sniffLen]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return fileBinary
	}
	if hasImageSig(data) {
		return fileImage
	}
	return fileText
}

// hasImageSig reports whether data begins with a common image magic number.
func hasImageSig(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	sig := map[string]bool{
		string([]byte{0x89, 'P', 'N', 'G'}): true,
		string([]byte{'R', 'I', 'F', 'F'}):  true, // webp/avi container start
	}
	if sig[string(data[:4])] {
		return true
	}
	return len(data) > 2 && data[0] == 0xff && data[1] == 0xd8 // jpeg
}

// numberLines renders data as line-numbered text starting at the given 1-based
// start line, for up to limit lines with each line clamped at MaxLineChars. It
// returns the rendered block and the last line emitted when output was truncated.
func numberLines(data []byte, start, limit int) (string, int) {
	lines := bytes.Split(bytes.ReplaceAll(data, []byte{'\r'}, nil), []byte{'\n'})
	var b strings.Builder
	end := min(start+limit-1, len(lines))
	if end < start { // offset past EOF
		return "", 0
	}
	for i := start - 1; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, capLine(string(lines[i])))
	}
	if end < len(lines) { // more lines remain past what was emitted
		return b.String(), end
	}
	return b.String(), 0
}
