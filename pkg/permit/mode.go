package permit

// Mode is the permission gate's operating level.
type Mode uint8

const (
	ModeAllowAll  Mode = iota // no gate; every call runs
	ModeAllowRead             // verifiably read-only calls run, everything else prompts (default)
	ModeAuto                  // allow-read plus model classification of unverifiable shell commands
	ModeAutoMCP               // auto plus model classification of MCP/extension tool calls with their metadata
	ModeBlockAll              // nothing writes or reads without a prompt; ! lines exempt
)

// ParseMode maps the config string to its Mode. The empty value means default.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "", "allow-read":
		return ModeAllowRead, true
	case "allow-all":
		return ModeAllowAll, true
	case "auto":
		return ModeAuto, true
	case "auto+mcp":
		return ModeAutoMCP, true
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
	case ModeAutoMCP:
		return "auto+mcp"
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
	case ModeAutoMCP:
		return "auto+"
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
		return ModeAutoMCP
	case ModeAutoMCP:
		return ModeAllowAll
	case ModeAllowAll:
		return ModeBlockAll
	case ModeBlockAll:
		return ModeAllowRead
	default:
		return ModeAllowRead
	}
}

// all returns whether the mode lets everything run without a gate.
func (m Mode) allowsEverything() bool { return m == ModeAllowAll }
