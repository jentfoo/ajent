package tui

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pngImage returns a small valid png's bytes.
func pngImage(t *testing.T) []byte {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 6))))
	return b.Bytes()
}

func TestDetectImageProtocol(t *testing.T) {
	t.Parallel()

	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}

	t.Run("kitty by term", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageKitty, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-kitty"}), true))
	})
	t.Run("kitty by window id", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageKitty, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color", "KITTY_WINDOW_ID": "1"}), true))
	})
	t.Run("iterm2 by term program", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageITerm2, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "iTerm.app"}), true))
	})
	t.Run("wezterm is iterm2", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageITerm2, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color", "WEZTERM_EXECUTABLE": "/usr/bin/wezterm"}), true))
	})
	t.Run("windows terminal is none", func(t *testing.T) {
		t.Parallel()
		// WT speaks sixel, not OSC 1337: advertising iterm2 would dump megabytes
		// of base64 into a terminal that ignores it
		assert.Equal(t, ImageNone, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color", "WT_SESSION": "x"}), true))
	})
	t.Run("ghostty and konsole are kitty", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageKitty, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color", "GHOSTTY_RESOURCES_DIR": "/usr/share/ghostty"}), true))
		assert.Equal(t, ImageKitty, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color", "KONSOLE_VERSION": "220403"}), true))
	})
	t.Run("unknown term is none", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageNone, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-256color"}), true))
	})
	t.Run("multiplexer is none", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageNone, DetectImageProtocol("", env(map[string]string{
			"TERM": "xterm-kitty", "TMUX": "/tmp/tmux,default"}), true))
		assert.Equal(t, ImageNone, DetectImageProtocol("", env(map[string]string{"TERM": "screen-256color"}), true))
	})
	t.Run("not a tty is none", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageNone, DetectImageProtocol("", env(map[string]string{"TERM": "xterm-kitty"}), false))
	})
	t.Run("explicit override wins", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, ImageKitty, DetectImageProtocol("kitty", env(map[string]string{"TERM": "screen"}), true))
		assert.Equal(t, ImageITerm2, DetectImageProtocol("iterm2", env(nil), true))
		assert.Equal(t, ImageNone, DetectImageProtocol("none", env(map[string]string{"TERM": "xterm-kitty"}), true))
	})
}

func TestResolveImageProtocol(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]ImageProtocol{
		"none":   ImageNone,
		"kitty":  ImageKitty,
		"iterm2": ImageITerm2,
	} {
		p, explicit, ok := ResolveImageProtocol(name)
		assert.True(t, ok, name)
		assert.True(t, explicit, name)
		assert.Equal(t, want, p, name)
	}
	_, explicit, ok := ResolveImageProtocol("auto")
	assert.True(t, ok)
	assert.False(t, explicit) // auto means detect
	_, _, ok = ResolveImageProtocol("sixel")
	assert.False(t, ok)
}

func TestImageRowsAndPlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("no protocol renders placeholder", func(t *testing.T) {
		t.Parallel()
		l := histLine{image: &histImage{rows: 1, text: "[image 640x480, 12kb]"}}
		rows := l.rows(80)
		assert.Equal(t, []string{"[image 640x480, 12kb]"}, rows)
	})

	t.Run("sequence rows pad to height", func(t *testing.T) {
		t.Parallel()
		l := histLine{image: &histImage{seq: "\x1b_Ga=T,f=100\x1b\\", rows: 4, text: "[image]"}}
		rows := l.rows(80)
		assert.Len(t, rows, 4)
		assert.Equal(t, "\x1b_Ga=T,f=100\x1b\\", rows[0])
		assert.Empty(t, strings.Join(rows[1:], ""))
	})
}

// renderers must drive this end to end: rows() alone passing is exactly how the
// placeholder-only case once slipped through commit.
func TestCommitRendersImagePlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("inline", func(t *testing.T) {
		t.Parallel()
		v := newVT(40, 12)
		r := newTestInline(v)
		r.commit([]histLine{{image: &histImage{rows: 1, text: "[image 640x480, 12kb]"}}})
		assert.Contains(t, v.Screen(), "[image 640x480, 12kb]")
	})

	t.Run("plain", func(t *testing.T) {
		t.Parallel()
		var buf strings.Builder
		p := &plainRenderer{out: &buf}
		p.commit([]histLine{{image: &histImage{rows: 1, text: "[image 640x480, 12kb]"}}})
		assert.Equal(t, "[image 640x480, 12kb]\n", buf.String())
	})

	t.Run("inline sequence goes out verbatim", func(t *testing.T) {
		t.Parallel()
		var buf strings.Builder
		r := &inlineRenderer{t: &termState{out: recWriter{&buf}, fd: -1, width: 40, height: 12}}
		r.commit([]histLine{{image: &histImage{seq: "\x1b_Ga=T,f=100\x1b\\", rows: 2, text: "[image]"}}})
		assert.Contains(t, buf.String(), "\x1b_Ga=T,f=100\x1b\\")
	})
}

func TestKittyTransmit(t *testing.T) {
	t.Parallel()

	t.Run("single chunk", func(t *testing.T) {
		t.Parallel()
		seq := kittyTransmit(7, []byte("tiny"), 10, 5)
		assert.Equal(t, "\x1b_Ga=T,f=100,q=2,C=1,i=7,c=10,r=5,m=0;dGlueQ==\x1b\\", seq)
	})

	t.Run("chunked payload", func(t *testing.T) {
		t.Parallel()
		data := make([]byte, imagePayloadChunk) // 4096 bytes -> 5462 base64 bytes -> two chunks
		seq := kittyTransmit(9, data, 10, 5)
		assert.Equal(t, 2, strings.Count(seq, "\x1b_G"))
		// control keys ride the first chunk only; the continuation is bare m
		assert.Contains(t, seq, "a=T,f=100,q=2,C=1,i=9,c=10,r=5,m=1;")
		assert.Contains(t, seq, "\x1b_Gm=0;")
		assert.NotContains(t, seq, "a=p")
	})
}

func TestImageLine(t *testing.T) {
	t.Parallel()

	newUI := func(protocol ImageProtocol) *UI {
		return &UI{
			mode:   ModeInline,
			images: protocol,
			imgIDs: make(map[uint32]int),
			render: &inlineRenderer{t: &termState{out: io.Discard, fd: -1, width: 120, height: 60}},
		}
	}

	t.Run("kitty fits the viewport", func(t *testing.T) {
		t.Parallel()
		// 800x600 px at the default 10x20 cell: 80 cols fit 120, 30 rows fit
		// the half-viewport of 30 exactly, so nothing clamps
		l := newUI(ImageKitty).imageLine(Image{Data: []byte("x"), W: 800, H: 600})
		assert.Contains(t, l.image.seq, "c=80,r=30")
		assert.Equal(t, 30, l.image.rows)
	})

	t.Run("kitty clamps tall images", func(t *testing.T) {
		t.Parallel()
		// 200x2000 px: 20 cols, 100 rows must clamp to the 30-row half
		l := newUI(ImageKitty).imageLine(Image{Data: []byte("x"), W: 200, H: 2000})
		assert.Contains(t, l.image.seq, "c=6,r=30")
		assert.Equal(t, 30, l.image.rows)
	})

	t.Run("no protocol is placeholder only", func(t *testing.T) {
		t.Parallel()
		data := pngImage(t)
		l := newUI(ImageNone).imageLine(Image{Data: data, W: 800, H: 600})
		assert.Empty(t, l.image.seq)
		assert.Equal(t, 1, l.image.rows)
		// dims come from the header, not the caller's W/H
		assert.Equal(t, "[image 8x6, "+strutil.HumanSize(int64(len(data)))+"]", l.image.text)
	})
}

func TestIterm2Sequence(t *testing.T) {
	t.Parallel()
	seq := iterm2Sequence([]byte("abc"), 8, 4)
	assert.True(t, strings.HasPrefix(seq, "\x1b]1337;File=inline=1;doNotMoveCursor=1;size=3;width=8;height=4;preserveAspectRatio=0:"))
	assert.True(t, strings.HasSuffix(seq, "YWJj\a"))
}

func TestIsImageLine(t *testing.T) {
	t.Parallel()
	assert.True(t, isImageLine("\x1b_Ga=T,f=100,q=2\x1b\\"))
	assert.True(t, isImageLine("\x1b]1337;File=inline=1;size=3:abc\a"))
	assert.False(t, isImageLine("plain text"))
	assert.False(t, isImageLine("\x1b[31mstyled text\x1b[0m"))

	// an image sequence survives the wrap pass untouched
	line := "\x1b_Ga=T,f=100\x1b\\"
	assert.Equal(t, []string{line}, wrapLine(line, 10))
}

func TestCellSizeReport(t *testing.T) {
	t.Run("decode and apply", func(t *testing.T) {
		// CSI 6;20;10 t answers a CSI 16 t query: height 20, width 10
		k, n, ok := decodeKey([]byte("\x1b[6;20;10t"))
		require.True(t, ok)
		assert.Equal(t, keyCellSize, k.typ)
		assert.Equal(t, 20, k.row)
		assert.Equal(t, 10, k.col)
		assert.Equal(t, 10, n)

		u := &UI{}
		u.setCellSize(k.col, k.row)
		assert.Equal(t, 2, u.imageRowsFor(33)) // ceil(33/20)
		assert.Equal(t, 4, u.imageColsFor(33)) // ceil(33/10)
	})

	t.Run("unrelated reports ignored", func(t *testing.T) {
		k, _, ok := decodeKey([]byte("\x1b[8;24;80t")) // terminal size report
		require.True(t, ok)
		assert.Equal(t, keyIgnore, k.typ)
	})

	t.Run("fallback geometry", func(t *testing.T) {
		u := &UI{}
		assert.Equal(t, 1, u.imageRowsFor(0))
		assert.Equal(t, 1, u.imageColsFor(5)) // below one cell
	})
}

func TestFitCells(t *testing.T) {
	t.Parallel()
	t.Run("within viewport untouched", func(t *testing.T) {
		t.Parallel()
		cols, rows := fitCells(40, 20, 120, 60)
		assert.Equal(t, 40, cols)
		assert.Equal(t, 20, rows)
	})
	t.Run("wide clamps to columns", func(t *testing.T) {
		t.Parallel()
		cols, rows := fitCells(200, 50, 100, 60)
		assert.Equal(t, 100, cols)
		assert.Equal(t, 25, rows)
	})
	t.Run("tall clamps to viewport half", func(t *testing.T) {
		t.Parallel()
		cols, rows := fitCells(60, 100, 120, 60)
		assert.Equal(t, 18, cols)
		assert.Equal(t, 30, rows)
	})
	t.Run("unknown size untouched", func(t *testing.T) {
		t.Parallel()
		cols, rows := fitCells(500, 200, 0, 0)
		assert.Equal(t, 500, cols)
		assert.Equal(t, 200, rows)
	})
}
