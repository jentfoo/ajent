package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// val is one JSON value in an order preserving tree: a scalar, array or object.
type val struct {
	k   valKind
	raw json.RawMessage // kindScalar bytes
	obj *object         // kindObj children by key, insertion ordered
	arr []*val          // kindArr elements, insertion ordered
}

// valKind discriminates the three node shapes.
type valKind uint8

const (
	kindScalar valKind = iota
	kindObj
	kindArr
)

func (v *val) isObject() bool { return v != nil && v.k == kindObj }

// isNull reports whether v holds the JSON null literal.
func (v *val) isNull() bool {
	return v != nil && v.k == kindScalar && string(v.raw) == "null"
}

// object holds a JSON object's keys in first-seen order alongside their values.
type object struct {
	keys []string
	m    map[string]*val
}

// parseNode decodes the one JSON value spanning all of data, treating empty
// input as an empty object so SetKey can build layers from nothing. Numbers use
// UseNumber to preserve them. Trailing bytes are an error: a save rebuilt from
// the tree would silently drop anything past the top-level value.
func parseNode(data []byte) (*val, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	root, err := readVal(dec)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected data after the top-level value")
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	return root, nil
}

// readVal consumes exactly one JSON value from dec.
func readVal(dec *json.Decoder) (*val, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch d := tok.(type) {
	case json.Delim:
		switch d {
		case '{':
			o := &object{m: make(map[string]*val)}
			for dec.More() {
				kt, kerr := dec.Token()
				key, ok := kt.(string)
				if !ok || kerr != nil {
					return nil, fmt.Errorf("config: non-string object key %#v (tok=%T): %w", kt, kt, kerr)
				}
				child, err := readVal(dec)
				if err != nil {
					return nil, err
				}
				// a source file may repeat a key; keep the last value without
				// appending it twice, so Save emits it once
				if _, ok := o.m[key]; !ok {
					o.keys = append(o.keys, key)
				}
				o.m[key] = child
			}
			_, _ = dec.Token() // the closing '}'
			return &val{k: kindObj, obj: o}, nil
		case '[':
			var arr []*val
			for dec.More() {
				child, err := readVal(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, child)
			}
			_, _ = dec.Token()
			return &val{k: kindArr, arr: arr}, nil
		default:
			return nil, fmt.Errorf("config: unexpected delimiter %q", d)
		}
	default:
		b, err := json.Marshal(tok) // string/number/bool/null re-encode exactly
		if err != nil {
			return nil, err
		}
		return &val{k: kindScalar, raw: b}, nil
	}
}

// marshal renders the node as compact JSON preserving key order.
func (v *val) marshal() []byte {
	var sb strings.Builder
	v.write(&sb)
	return []byte(sb.String())
}

func (v *val) write(sb *strings.Builder) {
	switch v.k {
	case kindScalar:
		sb.Write(v.raw)
	case kindArr:
		sb.WriteByte('[')
		for i, c := range v.arr {
			if i > 0 {
				sb.WriteByte(',')
			}
			c.write(sb)
		}
		sb.WriteByte(']')
	case kindObj:
		sb.WriteByte('{')
		for i, k := range v.obj.keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			sb.Write(kb)
			sb.WriteByte(':')
			v.obj.m[k].write(sb)
		}
		sb.WriteByte('}')
	}
}

// indentJSON pretty prints compact JSON at two spaces.
func indentJSON(data []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// SetKey returns data with the dotted key set to value, preserving unknown keys
// and key order, while missing intermediate objects are created.
func SetKey(data []byte, key string, value any) ([]byte, error) {
	root, err := parseNode(data)
	if err != nil {
		return nil, err
	}
	if !root.isObject() {
		root = &val{k: kindObj, obj: &object{m: make(map[string]*val)}}
	}
	nv, err := valueNode(value)
	if err != nil {
		return nil, err
	}
	setPath(root.obj, strings.Split(key, "."), nv)
	out, err := indentJSON(root.marshal())
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteKey returns data with the dotted key removed and intermediate objects
// the removal left empty pruned. A missing key returns data unchanged.
func DeleteKey(data []byte, key string) ([]byte, error) {
	root, err := parseNode(data)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(key, ".")
	var containers []*object // containers[i] holds the object declaring parts[i]
	cur := root
	for _, seg := range parts {
		if !cur.isObject() {
			return data, nil // the path crosses a scalar, so the key cannot exist
		}
		child, ok := cur.obj.m[seg]
		if !ok {
			return data, nil
		}
		containers = append(containers, cur.obj)
		cur = child
	}
	dropKey(containers[len(parts)-1], parts[len(parts)-1])
	for i := len(parts) - 1; i > 0 && len(containers[i].keys) == 0; i-- {
		dropKey(containers[i-1], parts[i-1]) // prune the emptied subtree's parent
	}
	out, err := indentJSON(root.marshal())
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dropKey removes one key from an order-preserving object.
func dropKey(o *object, seg string) {
	delete(o.m, seg)
	if i := slices.Index(o.keys, seg); i >= 0 {
		o.keys = slices.Delete(o.keys, i, i+1)
	}
}

// valueNode converts any marshals into a node.
func valueNode(value any) (*val, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return parseNode(b)
}

// setPath folds nv in at the object path parts, creating intermediates as needed.
func setPath(o *object, parts []string, nv *val) {
	seg := parts[0]
	if len(parts) == 1 {
		if _, ok := o.m[seg]; !ok {
			o.keys = append(o.keys, seg)
		}
		o.m[seg] = nv
		return
	}
	child, ok := o.m[seg]
	if !ok || !child.isObject() {
		child = &val{k: kindObj, obj: &object{m: make(map[string]*val)}}
		if !ok {
			o.keys = append(o.keys, seg)
		}
		o.m[seg] = child
	}
	setPath(child.obj, parts[1:], nv)
}

// lookupNode walks parts from cur, returning the node at the end or nil.
func lookupNode(cur *val, parts []string) *val {
	for _, seg := range parts {
		if !cur.isObject() {
			return nil
		}
		next, ok := cur.obj.m[seg]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}
