package tui

import (
	"math/rand"

	"github.com/jentfoo/ajent/pkg/img"
	"github.com/jentfoo/ajent/pkg/llm"
)

// Image is one image a caller wants committed to history.
type Image struct {
	Data      []byte // encoded image bytes
	MediaType string // image/png, image/jpeg, ...
	Label     string // short origin label for the placeholder, e.g. a path
	W, H      int    // pixel dimensions from the header, 0 when unknown
}

// String renders the honest placeholder, the one llm.ImagePlaceholder form
// shared by replay and MCP display.
func (im Image) String() string { return llm.ImagePlaceholder(im.Data, im.Label) }

// imageLine builds one committed image line: a protocol sequence sized to the
// cell grid where the mode can draw, else a placeholder-only line. Images only
// commit into inline history: it is the one mode that never repaints committed
// rows, so protocols without placement deletion (and repaints at all) cannot
// stack copies. Caller holds the lock.
func (u *UI) imageLine(im Image) histLine {
	if u.images == ImageNone || u.mode != ModeInline {
		return histLine{image: &histImage{rows: 1, text: im.String()}}
	}
	tw, th := u.render.size()
	cols, rows := fitCells(u.imageColsFor(im.W), u.imageRowsFor(im.H), tw, th)
	var seq string
	switch u.images {
	case ImageKitty:
		// f=100 names PNG: JPEG payloads ride only where the terminal sniffs
		// them (kitty, WezTerm), so convert anything else rather than risk a
		// silent rejection on Ghostty or Konsole
		data := im.Data
		if im.MediaType != "image/png" {
			if b, ok := img.ToPNG(im.Data); ok {
				data = b
			}
		}
		id := u.nextImageID()
		u.imgIDs[id] = rows
		seq = kittyTransmit(id, data, cols, rows)
	case ImageITerm2:
		seq = iterm2Sequence(im.Data, cols, rows)
	}
	return histLine{image: &histImage{seq: seq, rows: rows, text: im.String()}}
}

// nextImageID mints a fresh kitty image id, random first value then
// increasing, so ids never collide with ones a terminal still holds.
func (u *UI) nextImageID() uint32 {
	if u.imgSeq == 0 {
		u.imgSeq = rand.Uint32()
		if u.imgSeq == 0 {
			u.imgSeq = 1
		}
	}
	u.imgSeq++
	if len(u.imgIDs) >= maxTrackedImages {
		u.releaseOldestImage()
	}
	return u.imgSeq
}

// maxTrackedImages bounds the placement registry; dropping the oldest frees
// its terminal-side data through the deletion sequence.
const maxTrackedImages = 32

// releaseOldestImage forgets and deletes the oldest tracked kitty image.
// Caller holds the lock.
func (u *UI) releaseOldestImage() {
	var oldest uint32
	for id := range u.imgIDs {
		if oldest == 0 || id < oldest {
			oldest = id
		}
	}
	if oldest == 0 {
		return
	}
	delete(u.imgIDs, oldest)
	u.render.query(kittyDelete(oldest))
}

// watchCells consumes cell-size replies until the input closes.
func (u *UI) watchCells() {
	for c := range u.reader.cells {
		u.mu.Lock()
		u.setCellSize(c[0], c[1])
		u.mu.Unlock()
	}
}
