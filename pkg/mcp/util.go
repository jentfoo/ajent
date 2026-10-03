package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// jsonNull is the literal for an absent JSON value, compared against RawMessage.
const jsonNull = "null"

// maxFlexMS bounds the millisecond form of a FlexDuration so converting it to a
// time.Duration cannot overflow int64 nanoseconds.
const maxFlexMS = float64(math.MaxInt64 / time.Millisecond)

// FlexDuration is a config duration that accepts either a JSON number of
// milliseconds (the common MCP client convention) or a Go duration string like
// "60s". It marshals back to the millisecond form.
type FlexDuration time.Duration

// UnmarshalJSON decodes a numeric timeout in milliseconds or a duration string.
func (d *FlexDuration) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == jsonNull {
		*d = 0
		return nil
	}
	if b[0] == '"' { // quoted: a Go duration string
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return errors.New("timeout must be a millisecond number or duration string")
		}
		dur, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid timeout %q", s)
		}
		*d = FlexDuration(dur)
		return nil
	}
	var ms float64 // unquoted: milliseconds
	if err := json.Unmarshal(b, &ms); err != nil {
		return errors.New("timeout must be a millisecond number or duration string")
	}
	if ms < 0 || ms >= maxFlexMS {
		return fmt.Errorf("timeout %v ms out of range, want 0 to %d", ms, int64(maxFlexMS))
	}
	*d = FlexDuration(time.Duration(int64(ms)) * time.Millisecond)
	return nil
}

// FlexStrings is a config string list that also accepts a JSON boolean. A bare
// true expands to "*" so every tool matches, and false or absent yields nothing.
type FlexStrings []string

// UnmarshalJSON decodes an array of globs, a single glob, or a boolean.
func (f *FlexStrings) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == jsonNull {
		*f = nil
		return nil
	}
	switch string(b) { // bare tokens only; a quoted "true"/"false" is a glob
	case "true":
		*f = []string{"*"} // mark every tool read-only
		return nil
	case "false":
		*f = nil
		return nil
	}
	const shape = "readOnly must be a bool or a list of tool name globs"
	switch b[0] {
	case '[':
		var list []string
		if err := json.Unmarshal(b, &list); err != nil {
			return errors.New(shape)
		}
		*f = list
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return errors.New(shape)
		}
		*f = []string{s}
	default:
		return errors.New(shape)
	}
	return nil
}

// MarshalJSON renders a config duration as a millisecond count. Test support:
// configuration files are read-only so production never writes one, but round-trip
// tests need the numeric-ms encoding to stay faithful (the internal value is ns).
func (d FlexDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d) / time.Millisecond))
}
