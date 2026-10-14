package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// probeFile reports path's kind and size info alongside its content. Kind comes
// from the first sniffLen bytes: a NUL byte there marks binary, while a known
// image signature marks an image. Text and image return every byte; binary
// returns none, so refusing a giant file never loads one.
func probeFile(path string) (data []byte, info os.FileInfo, kind fileKind, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fileText, err
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil {
		return nil, nil, fileText, err
	}
	head, err := readSniff(f)
	if err != nil {
		return nil, nil, fileText, err
	}
	kind = detect(head)
	if kind == fileBinary {
		return nil, info, kind, nil // classified from the header alone
	}
	// text and image callers need every byte; continue on the same handle so
	// classification and content come from one read of the file
	var buf bytes.Buffer // accumulates head + remainder into one backing array
	buf.Write(head)
	if _, err = io.Copy(&buf, f); err != nil {
		return nil, nil, fileText, err
	}
	return buf.Bytes(), info, kind, nil
}

const sniffLen = 8 << 10

// readSniff reads up to sniffLen bytes from r. A file shorter than the window
// ends the read rather than erroring.
func readSniff(r io.Reader) ([]byte, error) {
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

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

// numberLines renders line-numbered text from start for up to limit lines or
// maxBytes (when positive), always emitting one line. Reports lastEmitted,
// truncatedAt when a bound cut the window, and total. An explicitLimit that is
// reached counts as complete: leftover past it reports no truncation.
func numberLines(data []byte, start, limit int, maxBytes int, explicitLimit bool) (out string, lastEmitted, truncatedAt, total int) {
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
	if end < total && !explicitLimit { // more lines remain past what was emitted
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
// existing line ending, always "\r\n" or "\n".
func restoreLineEndings(s, ending string) string {
	if ending == "\r\n" && strings.Contains(s, "\n") {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}
