package app

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// BenchmarkHubContextPush measures the editor keystroke's context push through
// the hub sink: the no-listener shape an interactive run without control pays
// per key, and the published shape one attached controller adds.
func BenchmarkHubContextPush(b *testing.B) {
	term, _ := pipeUI(b)
	hub := newTestHub(term)
	sink := hub.sink()
	c := tokens.ContextState{Used: 4200, Window: 200000, Reserve: 16000, Compact: 150000}

	b.Run("no_listeners", func(b *testing.B) {
		for b.Loop() {
			sink.Context(c)
		}
	})

	b.Run("one_listener", func(b *testing.B) {
		l, _ := hub.evListen(func(*evFrame) {})
		b.Cleanup(func() { hub.evUnlisten(l) })
		for b.Loop() {
			sink.Context(c)
		}
	})
}

// BenchmarkControlFanout measures one large event reaching several connections:
// the hub encodes and stamps once, every writer stages the shared frame, and
// each drain writes the one encoded line.
func BenchmarkControlFanout(b *testing.B) {
	term, _ := pipeUI(b)
	payload := ctl.TextData{Delta: strings.Repeat("x", 64<<10)}

	for _, observers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("observers_%d", observers), func(b *testing.B) {
			hub := newTestHub(term)
			conns := make([]*controlConn, observers)
			for i := range conns {
				conns[i] = newBenchConn()
				hub.evListen(conns[i].deliver)
			}
			for b.Loop() {
				hub.publish(ctl.EventTextDelta, payload)
				for _, c := range conns {
					require.NoError(b, c.drain())
				}
			}
		})
	}
}

// benchConn is a net.Conn that discards writes, so a drain measures encode and
// buffer work, never socket transport.
type benchConn struct{}

func (benchConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (benchConn) Write(p []byte) (int, error)      { return len(p), nil }
func (benchConn) Close() error                     { return nil }
func (benchConn) LocalAddr() net.Addr              { return nil }
func (benchConn) RemoteAddr() net.Addr             { return nil }
func (benchConn) SetDeadline(time.Time) error      { return nil }
func (benchConn) SetReadDeadline(time.Time) error  { return nil }
func (benchConn) SetWriteDeadline(time.Time) error { return nil }

// newBenchConn builds one writer-only connection over a discard conn.
func newBenchConn() *controlConn {
	nc := benchConn{}
	return &controlConn{
		conn:  nc,
		bw:    bufio.NewWriterSize(nc, 4096),
		done:  make(chan struct{}),
		wrote: make(chan struct{}),
		kick:  make(chan struct{}, 1),
	}
}
