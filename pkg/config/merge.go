package config

import (
	"bytes"
	"encoding/json"
	"maps"
)

// MergeObjects deep merges JSON objects, later layers winning per key. Nested
// objects merge; arrays and scalars are replaced. Empty layers are skipped.
func MergeObjects(layers ...[]byte) ([]byte, error) {
	var out map[string]any
	for _, data := range layers {
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var m map[string]any
		if err := decodeJSON(data, &m); err != nil {
			return nil, err
		} else if out == nil {
			out = m
			continue
		}
		mergeInto(out, m)
	}
	if out == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(out)
}

// mergeInto folds over into dst, recursing through nested objects.
func mergeInto(dst, over map[string]any) {
	for k, ov := range over {
		dv, ok := dst[k]
		if !ok {
			dst[k] = ov
			continue
		}
		dm, dok := dv.(map[string]any)
		om, ook := ov.(map[string]any)
		if dok && ook {
			merged := maps.Clone(dm)
			mergeInto(merged, om)
			dst[k] = merged
		} else {
			dst[k] = ov
		}
	}
}

// decodeJSON unmarshals data preserving number precision.
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
