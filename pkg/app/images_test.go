package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// slotPNG returns a small valid png for slot tests.
func slotPNG(t *testing.T) []byte {
	t.Helper()

	return slotSizedPNG(t, 6, 4)
}

// slotSizedPNG returns a valid png of the given dimensions.
func slotSizedPNG(t *testing.T, w, h int) []byte {
	t.Helper()

	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{R: 1, G: 2, B: 3, A: 255})
		}
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	return b.Bytes()
}

func TestImageSlots(t *testing.T) {
	t.Run("store and take round trip", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		token, rejected := s.store(slotPNG(t))
		assert.Empty(t, rejected)
		assert.Regexp(t, `^\[image #\d+]$`, token)

		text, blocks, notices := s.take("look at " + token + " please")
		assert.Equal(t, "look at  please", text)
		require.Len(t, blocks, 1)
		_, isImg := blocks[0].(llm.ImageBlock)
		assert.True(t, isImg)
		assert.Empty(t, notices)

		// a taken token reverts to orphan: dropped with a notice, not silently kept
		again, blocks, notices := s.take("again " + token)
		assert.Equal(t, "again ", again)
		assert.Empty(t, blocks)
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "no longer attached")
	})

	t.Run("multiple slots in order", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		t1, _ := s.store(slotPNG(t))
		t2, _ := s.store(slotPNG(t))

		text, blocks, _ := s.take(t1 + " then " + t2)
		assert.Equal(t, " then ", text)
		require.Len(t, blocks, 2) // text order: token position decides, not slot age
	})

	// text order even when the newer slot's token comes first in the line
	t.Run("blocks follow text order", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		wide, _ := s.store(slotSizedPNG(t, 40, 4))
		tall, _ := s.store(slotSizedPNG(t, 4, 40))

		_, blocks, _ := s.take(tall + " before " + wide)
		require.Len(t, blocks, 2)
		first, ok := blocks[0].(llm.ImageBlock)
		require.True(t, ok)
		w, h, _ := llm.ImageDims(first.Data)
		assert.Equal(t, 40, h) // the tall one's payload, named first in the text
		assert.Equal(t, 4, w)
	})

	t.Run("refused store inserts nothing", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		token, rejected := s.store([]byte("not an image"))
		assert.Empty(t, token)
		assert.Contains(t, rejected, "rejected")
		_, blocks, _ := s.take("no tokens here")
		assert.Empty(t, blocks)
	})

	t.Run("refile keeps queued blocks alive", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		blocks := llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: slotPNG(t)}}

		token := s.refile(blocks)
		assert.Regexp(t, `^\[image #\d+]$`, token)
		text, taken, _ := s.take("again " + token)
		assert.Equal(t, "again ", text)
		assert.Equal(t, blocks, taken)
		assert.Empty(t, s.refile(nil))
	})

	t.Run("orphaned tokens drop with a notice", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		text, blocks, notices := s.take("[image #99] and [image #7]")
		assert.Equal(t, " and ", text)
		assert.Empty(t, blocks)
		assert.Len(t, notices, 2)
	})

	t.Run("slots cap with fifo eviction", func(t *testing.T) {
		s := &imageSlots{slots: make(map[int]llm.BlockList)}
		var first string
		for range maxImageSlots {
			token, _ := s.store(slotPNG(t))
			if first == "" {
				first = token
			}
		}
		overflow, _ := s.store(slotPNG(t)) // evicts the first slot
		text, blocks, notices := s.take(first + " " + overflow)
		assert.NotContains(t, text, first)
		assert.Len(t, blocks, 1) // only the survivor's blocks
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "no longer attached")
	})
}

// Serial: swaps the package probe list and the no-backend cache.
func TestCaptureClipboardImage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based fakes are unix-only")
	}
	_, err := exec.LookPath("sh")
	require.NoError(t, err)
	clipNoBackend.Store(false)
	t.Cleanup(func() { clipNoBackend.Store(false) })
	realProbes := clipboardProbes
	t.Cleanup(func() { clipboardProbes = realProbes })

	t.Run("success stores a slot", func(t *testing.T) {
		_, base64Err := exec.LookPath("base64")
		if base64Err != nil {
			t.Skip("base64 not on PATH")
		}
		clipboardProbes = func() []clipCmd {
			return []clipCmd{{"sh", []string{"-c",
				"echo " + base64.StdEncoding.EncodeToString(slotPNG(t)) + " | base64 -d"}}}
		}
		token, notice := captureClipboardImage(t.Context())
		assert.Empty(t, notice)
		assert.Regexp(t, `^\[image #\d+]$`, token)
	})

	t.Run("over limit refused with notice", func(t *testing.T) {
		clipboardProbes = func() []clipCmd {
			return []clipCmd{{"sh", []string{"-c", "head -c 33554433 /dev/zero"}}} // one past the cap
		}
		token, notice := captureClipboardImage(t.Context())
		assert.Empty(t, token)
		assert.Contains(t, notice, "read limit")
	})

	t.Run("stderr noise stays off the tty", func(t *testing.T) {
		clipboardProbes = func() []clipCmd {
			return []clipCmd{{"sh", []string{"-c", "echo probe-noise >&2; exit 1"}}}
		}
		// a failing reader yields nothing: no slot, no notice (installed but empty)
		token, notice := captureClipboardImage(t.Context())
		assert.Empty(t, token)
		assert.Empty(t, notice)
	})
}

func TestImageToken(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "[image #12]", imageToken(12))
}

func TestRestoredLabel(t *testing.T) {
	t.Run("plain label untouched", func(t *testing.T) {
		it := steerItem{label: "plain text"}
		assert.Equal(t, "plain text", restoredLabel(it))
	})

	t.Run("blocks get a fresh token", func(t *testing.T) {
		blocks := llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: slotPNG(t)}}
		it := steerItem{label: "what is this", input: agent.Input{Blocks: blocks}}
		label := restoredLabel(it)
		assert.True(t, strings.HasPrefix(label, "[image #"))
		assert.Contains(t, label, "what is this")
	})
}

// Serial: flips the package-wide images gate. Resuming a transcript saved
// with images.block true must re-apply it, not leave the config default live.
func TestResumeAppliesImagesBlock(t *testing.T) {
	tools.SetImagesEnabled(true)
	t.Cleanup(func() { tools.SetImagesEnabled(true) })

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "a"}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	_, err = w.Append(session.TypeMessage, session.MessageData{
		Message: llm.Text(llm.RoleUser, "do the work"),
	})
	require.NoError(t, err)
	_, err = w.Append(session.TypeSettingChange, session.SettingData{
		Key: "images.block", Value: json.RawMessage(`true`),
	})
	require.NoError(t, err)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)

	st := &agent.State{Model: reg.Active()}
	toolsReg, err := tools.Builtins(tools.Options{SessionID: t.TempDir()})
	require.NoError(t, err)
	(&sessRec{w: w}).rebuild(set, ui, reg, st, toolsReg)

	assert.False(t, tools.ImagesEnabled())
}
