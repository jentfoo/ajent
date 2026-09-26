package config

import (
	"bytes"
	"encoding/json"
)

// decodeJSON unmarshals data preserving number precision.
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
