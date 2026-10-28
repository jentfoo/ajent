package ctl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Encoder writes one JSON value per line. Safe for concurrent use, so event
// fanout and request replies share one socket without interleaving.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

// NewEncoder wraps w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode marshals v and writes it as one line, flushing buffered writers.
func (e *Encoder) Encode(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err // an unencodable value is dropped, never a corrupted line
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, werr := e.w.Write(append(line, '\n'))
	return werr
}

// Decoder reads one JSON value per line, tolerating blank lines. Line length
// is unbounded: event payloads carry transcript entries.
type Decoder struct {
	r *bufio.Reader
}

// NewDecoder wraps r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r)}
}

// Decode reads the next line and unmarshals it into v. io.EOF reports a clean
// end of stream.
func (d *Decoder) Decode(v any) error {
	for {
		line, err := d.r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				return io.EOF
			}
			return err
		}
		trimmed := trimLine(line)
		if len(trimmed) > 0 {
			if jerr := json.Unmarshal(trimmed, v); jerr != nil {
				return fmt.Errorf("unparseable control line: %w", jerr)
			}
			return nil
		}
		if err != nil {
			return io.EOF // a final blank fragment ends the stream
		}
	}
}

// trimLine drops the newline and any carriage return a remote peer may add.
func trimLine(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}
