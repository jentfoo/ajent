package tools

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/jentfoo/ajent/pkg/img"
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

// detect classifies a buffer as text, binary or image. The image check runs
// before the NUL scan: png, webp, bmp and tiff all carry NUL bytes in their
// headers, so the byte scan alone would call them binary.
func detect(data []byte) (out fileKind) {
	if hasImageSig(data) {
		return fileImage
	}
	sniff := data
	if len(sniff) > sniffLen {
		sniff = sniff[:sniffLen]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return fileBinary
	}
	return fileText
}

// hasImageSig reports whether the registered decoders accept data's header as
// an image, so look-alike text ("BMW ...") and sibling RIFF files (wav, avi)
// stay out. The bare jpeg marker stands in for it: a jpeg whose frame header
// sits beyond the sniff window fails DecodeConfig, and 0xff 0xd8 cannot start
// text.
func hasImageSig(data []byte) bool {
	if _, ok := img.Sniff(data); ok {
		return true
	}
	return len(data) >= 2 && data[0] == 0xff && data[1] == 0xd8
}

// numberedLinePrefix is what numberLines writes ahead of every line: a six-wide
// line number and a tab.
const numberedLinePrefix = 7

// ReadBytes reports how many bytes a read of a measured file injects: its content
// plus the line-number prefix numberLines writes, bounded by the tool's line
// limit. It lets a caller size an injected read before running it. A measurement
// that skipped counting lines (one above the measure ceiling) reports its bytes
// alone.
func ReadBytes(m Measurement) int64 {
	lines := int64(m.Lines)
	if limit := int64(ReadFileLimit().Lines); limit > 0 && lines > limit {
		lines = limit
	}
	return m.Bytes + lines*numberedLinePrefix
}

// numberLines renders data as line-numbered text from the 1-based start line,
// stopping at limit lines or maxBytes of rendered output (when positive); at
// least one line is always emitted. It reports lastEmitted (the highest line
// rendered), truncatedAt (the last line rendered when a bound cut the window,
// zero when everything fit), and the file's total line count.
func numberLines(data []byte, start, limit, maxBytes int) (out string, lastEmitted, truncatedAt, total int) {
	lines := bytes.Split([]byte(normalizeToLF(string(data))), []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 { // drop the element a trailing newline leaves
		lines = lines[:len(lines)-1]
	}
	total = len(lines)
	var b strings.Builder
	end := min(start+limit-1, total)
	for i := start - 1; i < end; i++ {
		line := capLine(string(lines[i]))
		if len(line) < len(lines[i]) { // capped: say so, offset cannot reach the rest
			line += " ... [line truncated]"
		}
		if b.Len() > 0 && maxBytes > 0 && b.Len()+len(line)+1 > maxBytes {
			return b.String(), i, i, total // byte bound: whole lines only
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if start > total {
		return "", 0, 0, total // offset past EOF
	}
	if end < total { // more lines remain past what was emitted
		return b.String(), end, end, total
	}
	return b.String(), end, 0, total
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
