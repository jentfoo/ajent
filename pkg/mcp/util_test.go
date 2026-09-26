package mcp

import (
	"encoding/json"
	"time"
)

// MarshalJSON renders a config duration as a millisecond count. Test support:
// configuration files are read-only so production never writes one, but round-trip
// tests need the numeric-ms encoding to stay faithful (the internal value is ns).
func (d FlexDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d) / time.Millisecond))
}
