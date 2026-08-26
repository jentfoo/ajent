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

// writePerm returns path's existing permission bits, or 0o644 for a new file.
// Overwrites must not silently widen (or narrow) an owner-only mode like 0o600.
func writePerm(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}

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
// offset, capping each line and the total to limit. It reports lastEmitted (the
// highest line rendered) and truncatedAt (the next offset) when more lines remain
// past what was emitted.
func numberLines(data []byte, start, limit int) (out string, lastEmitted, truncatedAt int) {
	lines := bytes.Split([]byte(normalizeToLF(string(data))), []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 { // drop the element a trailing newline leaves
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	var b strings.Builder
	end := min(start+limit-1, total)
	if end < start { // offset past EOF
		return "", 0, 0
	}
	for i := start - 1; i < end; i++ {
		line := capLine(string(lines[i]))
		if len(line) < len(lines[i]) { // capped: say so, offset cannot reach the rest
			line += " ... [line truncated]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if end < total { // more lines remain past what was emitted
		return b.String(), end, end
	}
	return b.String(), end, 0
}

// detectLineEnding returns the majority line ending of data: "\r\n" when CRLF
// pairs outnumber plain LF newlines, else "\n". A new or empty file gets LF.
func detectLineEnding(data []byte) string {
	var crlf, lf int
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		if i > 0 && data[i-1] == '\r' {
			crlf++
		} else {
			lf++
		}
	}
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

// normalizeToLF rewrites CRLF pairs to LF. A lone \r (legitimate mid-line in
// shell output) is left alone.
func normalizeToLF(s string) string {
	if !strings.Contains(s, "\r\n") {
		return s
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// restoreLineEndings rewrites LF to ending so a write matches the document's
// existing line ending; ending is always "\r\n" or "\n".
func restoreLineEndings(s, ending string) string {
	if ending == "\r\n" && strings.Contains(s, "\n") {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}
