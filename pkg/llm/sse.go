package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"time"
)

const (
	// DefaultMaxFrame bounds the data one frame may accumulate.
	DefaultMaxFrame = 1 << 20
	// DoneSentinel is the data payload OpenAI dialects end a stream with. The
	// reader reports it; acting on it is the caller's choice, since MCP does not
	// use it.
	DoneSentinel = "[DONE]"
)

// ErrFrameTooLarge is returned when a frame exceeds the configured bound.
var ErrFrameTooLarge = errors.New("llm: sse frame too large")

// Frame is one dispatched server sent event.
type Frame struct {
	Event string // the event field, empty when absent
	Data  []byte // data lines joined with newlines, without a trailing newline
	ID    string // the most recent id field
	Retry time.Duration
}

// IsDone reports the OpenAI style end of stream sentinel.
func (f Frame) IsDone() bool { return string(f.Data) == DoneSentinel }

// SSEReader parses a text/event-stream body. It knows nothing about any
// provider dialect, so the MCP HTTP transport can use it unchanged.
type SSEReader struct {
	br       *bufio.Reader
	maxFrame int
	lastID   string
	line     []byte
	err      error
	closed   bool
	src      io.Reader
}

// NewSSEReader reads frames from r. maxFrame bounds both a single line and the
// data one frame accumulates, so an unterminated stream cannot grow without
// limit; zero uses DefaultMaxFrame.
func NewSSEReader(r io.Reader, maxFrame int) *SSEReader {
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrame
	}
	return &SSEReader{br: bufio.NewReader(r), maxFrame: maxFrame, src: r}
}

// LastEventID returns the most recent id field, for Last-Event-ID resumption.
func (r *SSEReader) LastEventID() string { return r.lastID }

// Close closes the underlying reader when it is an io.Closer.
func (r *SSEReader) Close() error {
	r.closed = true
	if r.err == nil {
		r.err = io.EOF
	}
	if c, ok := r.src.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Next returns the next frame. It returns io.EOF at end of stream, ctx.Err()
// when ctx is done, and ErrFrameTooLarge past the bound. Comment lines and
// blocks carrying no data are consumed rather than returned, so a frame is
// never empty.
//
// A blocked Next unblocks only when the underlying reader does; wire ctx to it,
// which http.NewRequestWithContext does.
func (r *SSEReader) Next(ctx context.Context) (Frame, error) {
	if r.err != nil {
		return Frame{}, r.err
	} else if err := ctx.Err(); err != nil {
		return Frame{}, r.fail(err)
	}

	var f Frame
	var data []byte
	var haveData bool
	for {
		line, err := r.readLine()
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return Frame{}, r.fail(cerr)
			}
			return Frame{}, r.fail(err) // a partial frame at EOF is discarded
		}
		if len(line) == 0 {
			if !haveData {
				f, data = Frame{}, nil // a block with no data resets and does not dispatch
				continue
			}
			f.Data = bytes.TrimSuffix(data, []byte("\n"))
			f.ID = r.lastID
			return f, nil
		} else if line[0] == ':' {
			continue // comment, usually a heartbeat
		}

		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			f.Event = string(value)
		case "data":
			data = append(append(data, value...), '\n')
			haveData = true
			if len(data) > r.maxFrame {
				return Frame{}, r.fail(ErrFrameTooLarge)
			}
		case "id":
			if !bytes.ContainsRune(value, 0) { // a NUL in an id is ignored per spec
				r.lastID = string(value)
			}
		case "retry":
			if ms, err := strconv.Atoi(string(value)); err == nil && ms >= 0 {
				f.Retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
}

// fail latches err so every later Next reports it rather than resyncing on
// whatever follows.
func (r *SSEReader) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return r.err
}

// readLine returns the next line without its terminator, accepting LF, CRLF and
// a bare CR. A final line with no terminator is returned before io.EOF.
func (r *SSEReader) readLine() ([]byte, error) {
	if r.closed {
		return nil, io.EOF
	}
	r.line = r.line[:0]
	for {
		b, err := r.br.ReadByte()
		if err != nil {
			if len(r.line) > 0 && errors.Is(err, io.EOF) {
				return r.line, nil
			}
			return nil, err
		}
		switch b {
		case '\n':
			return r.line, nil
		case '\r':
			if nb, nerr := r.br.ReadByte(); nerr == nil && nb != '\n' {
				_ = r.br.UnreadByte()
			}
			return r.line, nil
		}
		r.line = append(r.line, b)
		if len(r.line) > r.maxFrame {
			return nil, ErrFrameTooLarge
		}
	}
}
