package llm

import (
	"strconv"
	"time"
)

// Duration is a time.Duration that reads from configuration as a Go duration
// string such as "5m" or "0s", or as a bare number of seconds.
type Duration time.Duration

// MarshalText encodes the duration as a Go duration string.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalText decodes a Go duration string or a number of seconds.
func (d *Duration) UnmarshalText(data []byte) error {
	s := string(data)
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(secs * float64(time.Second))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// durOr returns d as a time.Duration, or alt when d is unset. An explicit zero
// is honoured, so a caller can disable a bound the default would enable.
func durOr(d *Duration, alt time.Duration) time.Duration {
	if d == nil {
		return alt
	}
	return time.Duration(*d)
}

// dur returns a pointer to d, for building a Timeouts literal.
func dur(d time.Duration) *Duration {
	v := Duration(d)
	return &v
}
