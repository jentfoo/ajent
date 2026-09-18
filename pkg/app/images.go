package app

import (
	"context"
	"io"
	"maps"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/img"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tools"
)

// clipboardTimeout bounds one clipboard probe; a hung backend degrades to a
// silent no-op like every other clipboard failure.
const clipboardTimeout = 3 * time.Second

// clipboardLimit bounds what a clipboard reader may hand us, so no backend can
// stream unbounded bytes into a decoder.
const clipboardLimit = 32 << 20

// clipNoBackend caches a failed backend lookup for the session, so Ctrl+V on a
// box without readers costs one probe instead of one per press.
var clipNoBackend atomic.Bool

// clipCmd is one platform clipboard reader tried in order.
type clipCmd struct {
	name string   // executable looked up on PATH
	args []string // arguments producing image bytes on stdout
}

// clipboardProbes lists the readers for the running platform, best first. A
// missing tool or one that yields no image ends the probe silently. A var so
// tests can inject fake readers.
var clipboardProbes = func() []clipCmd {
	switch runtime.GOOS {
	case "darwin":
		return []clipCmd{
			{"pngpaste", []string{"-"}},
		}
	case "windows":
		return []clipCmd{
			{"powershell.exe", []string{"-NoProfile", "-Command",
				`Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $i=[System.Windows.Forms.Clipboard]::GetImage(); if($i){ $ms=New-Object IO.MemoryStream; $i.Save($ms,[Drawing.Imaging.ImageFormat]::Png); [Console]::OpenStandardOutput().Write($ms.ToArray(),0,$ms.Length) }`}},
		}
	default:
		return []clipCmd{
			{"wl-paste", []string{"--no-newline", "--type", "image/png"}},
			{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
			{"wl-paste", []string{"--no-newline", "--type", "image/bmp"}},
			{"xclip", []string{"-selection", "clipboard", "-t", "image/bmp", "-o"}},
		}
	}
}

// imageSlots holds images captured from the clipboard until the editor line
// naming them is submitted. The control loop writes, the driver loop reads.
type imageSlots struct {
	mu    sync.Mutex
	seq   int
	slots map[int]llm.BlockList
}

// pendingImages is the session's slot table.
var pendingImages = &imageSlots{slots: make(map[int]llm.BlockList)}

// maxImageSlots bounds the table: a token abandoned in a draft or on a shell
// line never calls take, so unbounded slots would pin their bytes for the
// session's whole life.
const maxImageSlots = 8

// imageToken wraps one slot id in the editor text.
func imageToken(id int) string { return "[image #" + strconv.Itoa(id) + "]" }

// imageTokenRe matches any image token, known or orphaned.
var imageTokenRe = regexp.MustCompile(`\[image #\d+\]`)

// store fits one clipboard payload and files its blocks under a fresh token.
// rejected names a refused image, ready to show the user.
func (s *imageSlots) store(data []byte) (token, rejected string) {
	blocks, err := tools.FitImage(data)
	if err != nil {
		return "", tools.FitImageError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.slots[s.seq] = blocks
	s.evictLocked()
	return imageToken(s.seq), ""
}

// refile files blocks under a fresh slot and returns its token, so recalled or
// aborted queue items keep their images; their original slots were consumed at
// submission. Empty blocks return "".
func (s *imageSlots) refile(blocks llm.BlockList) string {
	if len(blocks) == 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.slots[s.seq] = blocks
	s.evictLocked()
	return imageToken(s.seq)
}

// evictLocked drops the oldest slot once the table runs over. Caller holds mu.
func (s *imageSlots) evictLocked() {
	if len(s.slots) <= maxImageSlots {
		return
	}
	delete(s.slots, slices.Min(slices.Collect(maps.Keys(s.slots))))
}

// take extracts every token's blocks from text and removes the tokens, in
// text order, then drops any orphaned tokens (their slot gone, say after a
// rewind that consumed them) with one notice each. Unknown text is untouched.
func (s *imageSlots) take(text string) (string, llm.BlockList, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var blocks llm.BlockList
	notices := make([]string, 0, 2)
	for _, m := range imageTokenRe.FindAllString(text, -1) {
		id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(m, "[image #"), "]"))
		if b, ok := s.slots[id]; err == nil && ok {
			blocks = append(blocks, b...)
			delete(s.slots, id)
		} else {
			notices = append(notices, "image no longer attached: dropped "+m)
		}
		text = strings.Replace(text, m, "", 1)
	}
	return text, blocks, notices
}

// captureClipboardImage probes the clipboard and files an image slot. notice
// is empty on success: it names a refused image, or the missing readers once
// per session.
func captureClipboardImage(ctx context.Context) (token, notice string) {
	if clipNoBackend.Load() {
		return "", ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()
	var tried []string
	installed := false
	for _, c := range clipboardProbes() {
		if !slices.Contains(tried, c.name) {
			tried = append(tried, c.name)
		}
		if _, err := exec.LookPath(c.name); err != nil {
			continue
		}
		installed = true
		cmd := exec.CommandContext(probeCtx, c.name, c.args...)
		cmd.Stderr = io.Discard // probe noise never reaches the tty
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			continue
		}
		if err := cmd.Start(); err != nil {
			continue
		}
		out, _ := io.ReadAll(io.LimitReader(stdout, clipboardLimit+1))
		_ = cmd.Wait()
		if len(out) > clipboardLimit { // cap+1 arrived: payload continues past it
			return "", "clipboard image exceeds the " + strutil.HumanSize(clipboardLimit) + " read limit"
		}
		if _, ok := img.Sniff(out); ok {
			return pendingImages.store(out)
		}
	}
	if !installed {
		clipNoBackend.Store(true) // nothing installs mid-session; one notice is enough
		return "", "no clipboard image reader on PATH (tried " + strings.Join(tried, ", ") + ")"
	}
	return "", ""
}

// applyImagesBlock re-applies the block-images setting mid-session, so the
// /settings toggle takes effect without a restart.
func applyImagesBlock(set *config.Set) {
	tools.SetImagesEnabled(!set.Settings().Images.Block)
}
