package tui

import (
	"strings"
)

// ImageProtocol names a supported terminal image protocol.
type ImageProtocol uint8

// protocolNone is the configuration and display name for "no protocol".
const protocolNone = "none"

const (
	// ImageNone renders placeholders; no protocol detected or images forced off.
	ImageNone ImageProtocol = iota
	// ImageKitty is the kitty graphics protocol with placement deletion.
	ImageKitty
	// ImageITerm2 is the OSC 1337 inline file protocol, no deletion support.
	ImageITerm2
)

// String returns the protocol's configuration name.
func (p ImageProtocol) String() string {
	switch p {
	case ImageKitty:
		return "kitty"
	case ImageITerm2:
		return "iterm2"
	default:
		return protocolNone
	}
}

// ResolveImageProtocol maps a ui.images value onto a protocol. explicit is
// true for kitty/iterm2/none, false for ""/auto meaning detect from the
// environment. ok is false for an unknown name.
func ResolveImageProtocol(s string) (p ImageProtocol, explicit, ok bool) {
	switch s {
	case "", "auto":
		return ImageNone, false, true
	case "kitty":
		return ImageKitty, true, true
	case "iterm2":
		return ImageITerm2, true, true
	case protocolNone:
		return ImageNone, true, true
	default:
		return ImageNone, false, false
	}
}

// DetectImageProtocol picks the terminal image protocol: an explicit override,
// else detection from the environment. Multiplexed terminals report none,
// matching the conservative colour-profile default.
func DetectImageProtocol(want string, env func(string) string, isTTY bool) ImageProtocol {
	p, explicit, ok := ResolveImageProtocol(want)
	if !ok || explicit {
		return p
	}
	if !isTTY || multiplexed(env) {
		return ImageNone
	}
	term := env("TERM")
	termProgram := env("TERM_PROGRAM")
	switch {
	case strings.HasPrefix(term, "xterm-kitty"), env("KITTY_WINDOW_ID") != "",
		env("KITTY_PID") != "", env("GHOSTTY_RESOURCES_DIR") != "",
		strings.HasPrefix(term, "konsole"), env("KONSOLE_VERSION") != "":
		return ImageKitty
	case termProgram == "iTerm.app", termProgram == "WezTerm",
		env("WEZTERM_EXECUTABLE") != "", term == "mintty":
		// WezTerm handles the kitty protocol too, but inline history never
		// repaints, so the OSC form's missing deletion costs nothing there
		return ImageITerm2
	default:
		return ImageNone
	}
}
