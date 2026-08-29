package permit

// Mode is the permission gate's operating level.
type Mode uint8

const (
	ModeAllowAll  Mode = iota // no gate; every call runs
	ModeAllowRead             // verifiably read-only calls run, everything else prompts (default)
	ModeAuto                  // allow-read plus model classification of unverifiable shell and MCP/extension tool calls
	ModeAutoWrite             // auto plus writes confined to the workspace roots
	ModeBlockAll              // nothing writes or reads without a prompt; ! lines exempt
)

// ParseMode maps the config string to its Mode. The empty value means default.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "", "allow-read":
		return ModeAllowRead, true
	case "allow-all":
		return ModeAllowAll, true
	case "auto", "auto+mcp": // auto+mcp is legacy for auto
		return ModeAuto, true
	case "auto+write":
		return ModeAutoWrite, true
	case "block-all":
		return ModeBlockAll, true
	default:
		return 0, false
	}
}

// String returns the config name for a mode.
func (m Mode) String() string {
	switch m {
	case ModeAllowAll:
		return "allow-all"
	case ModeAllowRead:
		return "allow-read"
	case ModeAuto:
		return "auto"
	case ModeAutoWrite:
		return "auto+write"
	case ModeBlockAll:
		return "block-all"
	default:
		return ""
	}
}

// Short returns the status-segment label for a mode.
func (m Mode) Short() string {
	switch m {
	case ModeAllowAll:
		return "all"
	case ModeAllowRead:
		return "read"
	case ModeAuto:
		return "auto"
	case ModeAutoWrite:
		return "auto+w"
	case ModeBlockAll:
		return "block"
	default:
		return ""
	}
}

// Next returns the following mode in cycle order.
func (m Mode) Next() Mode {
	switch m {
	case ModeAllowRead:
		return ModeAuto
	case ModeAuto:
		return ModeAutoWrite
	case ModeAutoWrite:
		return ModeAllowAll
	case ModeAllowAll:
		return ModeBlockAll
	case ModeBlockAll:
		return ModeAllowRead
	default:
		return ModeAllowRead
	}
}

// allowsEverything reports whether every call runs ungated.
func (m Mode) allowsEverything() bool { return m == ModeAllowAll }

// allowsWrites reports whether write/edit run ungated within the workspace roots.
func (m Mode) allowsWrites() bool { return m == ModeAutoWrite }
