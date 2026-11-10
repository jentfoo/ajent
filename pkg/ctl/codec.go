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

// EncodeLine marshals v into one newline-terminated wire line.
func EncodeLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err // an unencodable value is dropped, never a corrupted line
	}
	return append(b, '\n'), nil
}

// Encode marshals v and writes it as one line.
func (e *Encoder) Encode(v any) error {
	line, err := EncodeLine(v)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, werr := e.w.Write(line)
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
		// drop the newline and any carriage return a remote peer may add
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > 0 {
			if jerr := json.Unmarshal(line, v); jerr != nil {
				return fmt.Errorf("unparseable control line: %w", jerr)
			}
			return nil
		}
		if err != nil {
			return io.EOF // a final blank fragment ends the stream
		}
	}
}
