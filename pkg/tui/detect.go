package tui

import (
	"strconv"
	"strings"
	"time"
)

const (
	// backgroundQuery asks for the terminal's default background (OSC 11). The
	// DA1 that follows is a fence: a terminal ignoring OSC 11 still answers it,
	// so an unsupported query costs one round trip instead of the full timeout.
	backgroundQuery = esc + "]11;?" + esc + "\\"
	attrsQuery      = csi + "c"
	// a terminal answering neither gets this much grace before we give up
	toneQueryTimeout = 150 * time.Millisecond
)

// DetectTone reports the terminal's background tone, ToneUnknown when it cannot
// be established. It queries the terminal, so it belongs on a startup path with
// no interaction in flight.
func (u *UI) DetectTone() Tone {
	if t := toneFromEnv(osEnv); t != ToneUnknown {
		return t
	} else if u.mode == ModePlain || u.reader == nil {
		return ToneUnknown
	}

	u.mu.Lock()
	u.render.query(backgroundQuery + attrsQuery)
	u.mu.Unlock()

	expired := make(chan struct{})
	deadline := u.afterDelay(toneQueryTimeout, func() { close(expired) })
	defer deadline.Stop()
	for {
		select {
		case spec, ok := <-u.reader.colors:
			if !ok {
				return ToneUnknown // input ended
			} else if tone, valid := parseBackground(spec); valid {
				return tone
			}
		case <-u.reader.attrs:
			return ToneUnknown // the fence answered, the query did not
		case <-expired:
			return ToneUnknown
		}
	}
}

// toneFromEnv classifies COLORFGBG ("fg;bg" or "fg;default;bg"), which several
// terminals set from their own theme.
func toneFromEnv(env func(string) string) Tone {
	fields := strings.Split(env("COLORFGBG"), ";")
	if len(fields) < 2 {
		return ToneUnknown
	}
	n, err := strconv.Atoi(strings.TrimSpace(fields[len(fields)-1]))
	if err != nil {
		return ToneUnknown
	}
	switch {
	case n == 7 || (n >= 9 && n <= 15):
		return ToneLight
	case n >= 0 && n <= 8:
		return ToneDark
	default:
		return ToneUnknown
	}
}

// parseBackground classifies an X11 color spec by relative luminance.
func parseBackground(spec string) (Tone, bool) {
	r, g, b, ok := parseColorSpec(strings.TrimSpace(spec))
	if !ok {
		return ToneUnknown, false
	} else if 0.2126*r+0.7152*g+0.0722*b > 0.5 {
		return ToneLight, true
	}
	return ToneDark, true
}

// parseColorSpec returns the components of "rgb:RR/GG/BB" or "#RRGGBB" scaled
// to 0..1, false when the spec is malformed.
func parseColorSpec(spec string) (r, g, b float64, ok bool) {
	if hex, cut := strings.CutPrefix(spec, "#"); cut {
		if len(hex) == 0 || len(hex)%3 != 0 {
			return 0, 0, 0, false
		}
		w := len(hex) / 3
		return rgbFractions(hex[:w], hex[w:2*w], hex[2*w:])
	}
	rest, cut := strings.CutPrefix(spec, "rgb:")
	if !cut {
		return 0, 0, 0, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	return rgbFractions(parts[0], parts[1], parts[2])
}

// rgbFractions scales three hex components to 0..1.
func rgbFractions(rs, gs, bs string) (r, g, b float64, ok bool) {
	var okR, okG, okB bool
	r, okR = hexFraction(rs)
	g, okG = hexFraction(gs)
	b, okB = hexFraction(bs)
	return r, g, b, okR && okG && okB
}

// hexFraction scales one hex component by its own width, so "ff" and "ffff"
// both read as full intensity.
func hexFraction(s string) (float64, bool) {
	if s == "" || len(s) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	return float64(v) / float64(int(1)<<(4*len(s))-1), true
}
