package clipboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackend swaps every platform seam for an in-memory one and restores them
// at cleanup. env answers writer display-variable lookups; nil means every
// variable unset. Serial: swaps package vars.
func fakeBackend(t *testing.T, list []writer, installed func(string) bool,
	run func(_ context.Context, w writer, text string) error, isRemote bool, env func(string) string) *bytes.Buffer {
	t.Helper()

	realWriters, realAvailable, realRun := writers, available, runWriter
	realRemote, realStdout, realEnv := remote, stdout, envValue
	t.Cleanup(func() {
		writers, available, runWriter = realWriters, realAvailable, realRun
		remote, stdout, envValue = realRemote, realStdout, realEnv
	})
	if env == nil {
		env = func(string) string { return "" }
	}
	var out bytes.Buffer
	writers = func() []writer { return list }
	available, runWriter = installed, run
	remote, stdout, envValue = func() bool { return isRemote }, &out, env
	return &out
}

func TestCopy(t *testing.T) {
	ok := func(context.Context, writer, string) error { return nil }

	t.Run("native_writer_wins", func(t *testing.T) {
		var got []string
		out := fakeBackend(t,
			[]writer{{"one", nil, nil}, {"two", nil, nil}},
			func(string) bool { return true },
			func(_ context.Context, w writer, text string) error {
				got = append(got, w.name+":"+text)
				if w.name == "one" {
					return errors.New("boom")
				}
				return nil
			},
			false, nil)

		require.NoError(t, Copy(t.Context(), "hello"))
		assert.Equal(t, []string{"one:hello", "two:hello"}, got)
		assert.Empty(t, out.String()) // local native win: no OSC 52
	})

	t.Run("remote_native_win_also_emits_osc52", func(t *testing.T) {
		var ran []string
		out := fakeBackend(t,
			[]writer{{"xclip", nil, []string{"DISPLAY"}}},
			func(string) bool { return true },
			func(_ context.Context, w writer, _ string) error { ran = append(ran, w.name); return nil },
			true,
			func(k string) string {
				if k == "DISPLAY" {
					return ":0"
				}
				return ""
			})

		require.NoError(t, Copy(t.Context(), "hello"))
		assert.Equal(t, []string{"xclip"}, ran)
		// the forwarded display may not be the user's clipboard: emit both
		assert.Equal(t, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte("hello"))+"\x07", out.String())
	})

	t.Run("oversized_osc_after_native_win_still_succeeds", func(t *testing.T) {
		out := fakeBackend(t,
			[]writer{{"pbcopy", nil, nil}},
			func(string) bool { return true },
			ok,
			true, nil)

		// the native write landed; the refused OSC 52 must not fail the copy
		require.NoError(t, Copy(t.Context(), strings.Repeat("x", maxEncoded)))
		assert.Empty(t, out.String())
	})

	t.Run("missing_writers_skipped", func(t *testing.T) {
		var ran []string
		fakeBackend(t,
			[]writer{{"absent", nil, nil}, {"present", nil, nil}},
			func(name string) bool { return name == "present" },
			func(_ context.Context, _ writer, _ string) error { ran = append(ran, "ran"); return nil },
			false, nil)

		require.NoError(t, Copy(t.Context(), "hello"))
		assert.Equal(t, []string{"ran"}, ran)
	})

	t.Run("display_gated_writer_skipped", func(t *testing.T) {
		var ran []string
		fakeBackend(t,
			[]writer{
				{"wl-copy", nil, []string{"WAYLAND_DISPLAY"}},
				{"xclip", nil, []string{"DISPLAY"}},
			},
			func(string) bool { return true },
			func(_ context.Context, w writer, _ string) error { ran = append(ran, w.name); return nil },
			false,
			func(k string) string {
				if k == "DISPLAY" {
					return ":0"
				}
				return ""
			})

		require.NoError(t, Copy(t.Context(), "hello"))
		assert.Equal(t, []string{"xclip"}, ran) // wl-copy gated: no Wayland
	})

	t.Run("all_installed_but_displayless_errors", func(t *testing.T) {
		var ran []string
		fakeBackend(t,
			[]writer{{"wl-copy", nil, []string{"WAYLAND_DISPLAY"}}},
			func(string) bool { return true },
			func(_ context.Context, _ writer, _ string) error { ran = append(ran, "ran"); return nil },
			false, nil)

		err := Copy(t.Context(), "hello")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "display session")
		assert.Empty(t, ran)
	})

	t.Run("local_without_writer_errors_with_guidance", func(t *testing.T) {
		out := fakeBackend(t,
			[]writer{{"xclip", nil, nil}},
			func(string) bool { return false },
			ok,
			false, nil)

		err := Copy(t.Context(), "hello")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "install xclip")
		assert.Empty(t, out.String())
	})

	t.Run("failing_writers_report_tried", func(t *testing.T) {
		fakeBackend(t,
			[]writer{{"one", nil, nil}, {"two", nil, nil}},
			func(string) bool { return true },
			func(context.Context, writer, string) error { return errors.New("boom") },
			false, nil)

		err := Copy(t.Context(), "hello")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tried one, two")
	})

	t.Run("remote_falls_back_to_osc52", func(t *testing.T) {
		out := fakeBackend(t,
			[]writer{{"xclip", nil, nil}},
			func(string) bool { return false },
			ok,
			true, nil)

		require.NoError(t, Copy(t.Context(), "hello"))
		assert.Equal(t, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte("hello"))+"\x07", out.String())
	})

	t.Run("cancelled_context_refuses", func(t *testing.T) {
		fakeBackend(t,
			[]writer{{"one", nil, nil}},
			func(string) bool { return true },
			ok,
			false, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		require.ErrorIs(t, Copy(ctx, "hello"), context.Canceled)
	})
}

func TestWriteOSC52(t *testing.T) {
	t.Run("payload_is_base64", func(t *testing.T) {
		var out bytes.Buffer
		real := stdout
		stdout = &out
		t.Cleanup(func() { stdout = real })

		require.NoError(t, writeOSC52("hi"))
		assert.True(t, strings.HasPrefix(out.String(), "\x1b]52;c;"))
		assert.True(t, strings.HasSuffix(out.String(), "\x07"))
		assert.Contains(t, out.String(), base64.StdEncoding.EncodeToString([]byte("hi")))
	})

	t.Run("oversized_payload_refuses", func(t *testing.T) {
		var out bytes.Buffer
		real := stdout
		stdout = &out
		t.Cleanup(func() { stdout = real })

		err := writeOSC52(strings.Repeat("x", maxEncoded))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OSC 52 limit")
		assert.Empty(t, out.String())
	})

	t.Run("write_failure_wrapped", func(t *testing.T) {
		real := stdout
		stdout = failingWriter{}
		t.Cleanup(func() { stdout = real })

		require.Error(t, writeOSC52("hi"))
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }
