package tools

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// FileKind classifies what a measured path holds: text, binary or image.
type FileKind uint8

const (
	KindText FileKind = iota
	KindBinary
	KindImage
)

// Measurement describes a path's shape without reading its whole content when
// it is large. Lines is zero when the file was not read (too big or not text).
type Measurement struct {
	Dir   bool
	Kind  FileKind
	Lines int
	Bytes int64
}

// Measure stats path and, when it is a small enough text file, counts its lines.
// A directory reports Dir with its byte size left zero. A file above
// MeasureCeiling reports its bytes and kind but never reads it, so annotating a
// giant file is itself bounded. A missing path returns the os error.
func Measure(path string) (Measurement, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Measurement{}, err
	}
	if info.IsDir() {
		return Measurement{Dir: true}, nil
	}
	var m Measurement
	m.Bytes = info.Size()
	// classify from a bounded sniff, never the whole file
	kind, err := sniffKind(path)
	if err != nil {
		// a read failure still yields the byte size; the model sees the path
		m.Kind = KindText
		return m, nil
	}
	m.Kind = kind
	if kind != KindText {
		return m, nil
	}
	if info.Size() > MeasureCeiling {
		return m, nil // too big to count lines; bytes already set
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m, nil // stat said it existed; treat a vanishing read as no lines
	}
	m.Lines = countLines(string(data))
	return m, nil
}

// sniffKind reads at most sniffLen bytes and classifies them. It never reads the
// whole file, so a large binary is not slurped just to be annotated.
func sniffKind(path string) (FileKind, error) {
	f, err := os.Open(path)
	if err != nil {
		return KindText, err
	}
	defer func() { _ = f.Close() }()
	sniff := make([]byte, sniffLen)
	n, err := io.ReadFull(f, sniff)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return KindText, err
	}
	switch detect(sniff[:n]) {
	case fileImage:
		return KindImage, nil
	case fileBinary:
		return KindBinary, nil
	default:
		return KindText, nil
	}
}

// HumanSize abbreviates a byte count the way annotations show it: 64kb, 1.2mb.
// It rounds to one decimal and drops a trailing .0.
func HumanSize(n int64) string {
	const (
		kb = 1024.0
		mb = 1024.0 * 1024.0
	)
	switch {
	case float64(n) >= mb:
		return trimSizeFloat(float64(n)/mb) + "mb"
	case float64(n) >= kb:
		return trimSizeFloat(float64(n)/kb) + "kb"
	default:
		return strconv.FormatInt(n, 10) + "b"
	}
}

// trimSizeFloat formats v to one decimal and drops a trailing .0.
func trimSizeFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}
