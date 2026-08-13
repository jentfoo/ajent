package config

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// object holds a JSON object's keys in first-seen order alongside their values.
type object struct {
	keys []string
	m    map[string]*val
}

// parseNode decodes one JSON value, treating empty input as an empty object so
// SetKey can build layers from nothing. Numbers use UseNumber to preserve them.
func parseNode(data []byte) (*val, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return readVal(dec)
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
				o.keys = append(o.keys, key)
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
// and key order; missing intermediate objects are created.
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

// GetKey returns the raw value at the dotted key.
func GetKey(data []byte, key string) (json.RawMessage, bool) {
	v := lookupNode(parseNodeOrNil(data), strings.Split(key, "."))
	if v == nil {
		return nil, false
	}
	return json.RawMessage(v.marshal()), true
}

// parseNodeOrNil returns the parsed root or nil when data is invalid.
func parseNodeOrNil(data []byte) *val {
	v, err := parseNode(data)
	if err != nil {
		return nil
	}
	return v
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

// removeKey drops key from o preserving the order of remaining keys.
func removeKey(o *object, key string) {
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	delete(o.m, key)
}
