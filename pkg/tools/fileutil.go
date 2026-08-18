package tools

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
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
	data, err = os.ReadFile(path)
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
// offset, capping each line and the total to limit. It also reports how many of
// the file's lines were emitted (linesRead) and the rune count of their capped
// content (charsRead), so read can show what was actually pulled in without
// exposing it.
func numberLines(data []byte, start, limit int) (out string, truncatedAt, linesRead, charsRead int) {
	lines := bytes.Split(bytes.ReplaceAll(data, []byte{'\r'}, nil), []byte{'\n'})
	var b strings.Builder
	end := min(start+limit-1, len(lines))
	if end < start { // offset past EOF
		return "", 0, 0, 0
	}
	for i := start - 1; i < end; i++ {
		line := capLine(string(lines[i]))
		charsRead += utf8.RuneCountInString(line)
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	linesRead = end - (start - 1)
	if end < len(lines) { // more lines remain past what was emitted
		return b.String(), end, linesRead, charsRead
	}
	return b.String(), 0, linesRead, charsRead
}
