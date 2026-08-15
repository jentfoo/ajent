package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-analyze/bulk"
)

// jsonNull is the literal for an absent JSON value, compared against RawMessage.
const jsonNull = "null"

// FlexDuration is a config duration that accepts either a JSON number of
// milliseconds (the convention other MCP clients such as pi use) or a Go
// duration string like "60s". It marshals back to the millisecond form.
type FlexDuration time.Duration

// UnmarshalJSON decodes a numeric timeout in milliseconds or a duration string.
func (d *FlexDuration) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == jsonNull {
		*d = 0
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s != string(b) { // quoted: a Go duration string
		dur, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid timeout %q", s)
		}
		*d = FlexDuration(dur)
		return nil
	}
	var ms float64 // unquoted: milliseconds, matching pi's numeric timeouts
	if err := json.Unmarshal(b, &ms); err != nil {
		return errors.New("timeout must be a millisecond number or duration string")
	}
	dur := time.Duration(ms) * time.Millisecond
	*d = FlexDuration(dur)
	return nil
}

// MarshalJSON renders the duration as a millisecond count.
func (d FlexDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d) / time.Millisecond))
}

// FlexStrings is a config string list that also accepts a JSON boolean. A bare
// true expands to "*" so every tool matches; false or absent yields nothing.
type FlexStrings []string

// UnmarshalJSON decodes an array of globs, a single glob, or a boolean.
func (f *FlexStrings) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == jsonNull {
		*f = nil
		return nil
	}
	s := strings.Trim(string(b), `"`)
	switch s {
	case "true":
		*f = []string{"*"} // mark every tool read-only
		return nil
	case "false", jsonNull:
		*f = nil
		return nil
	}
	if string(b) == s { // unquoted non-boolean: an array of globs
		var list []string
		if err := json.Unmarshal(b, &list); err != nil {
			return errors.New("readOnly must be a bool or a list of tool name globs")
		}
		*f = FlexStrings(list)
		return nil
	}
	*f = []string{s} // a single quoted glob
	return nil
}

// toSet builds a set from names for O(1) membership.
func toSet(names []string) map[string]struct{} { return bulk.SliceToSet(names) }
