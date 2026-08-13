package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Layer is one configuration source at a precedence position. Name identifies
// the layer for Explain ("default", "user", ...); Path names its file when it has
// one; Data holds its raw JSON.
type Layer struct {
	Name string
	Path string
	Data []byte
}

// Resolved is a merged configuration with per-leaf provenance so callers can say
// where each value came from. It wraps an order preserving tree plus the source
// map Merge built.
type Resolved struct {
	root *val
	src  map[string]string // dotted leaf path → layer name, last writer wins
}

// Bytes returns the merged JSON, indented at two spaces with key order kept.
func (r Resolved) Bytes() []byte {
	out, err := indentJSON(r.root.marshal())
	if err != nil {
		return r.root.marshal()
	}
	return out
}

// Explain returns the resolved value at key plus the layer it came from. found is
// false when no layer declares the key.
func (r Resolved) Explain(key string) (json.RawMessage, string, bool) {
	v := lookupNode(r.root, splitPath(key))
	if v == nil {
		return nil, "", false
	}
	return json.RawMessage(v.marshal()), r.src[key], true
}

// Source returns the layer name that supplied key's value.
func (r Resolved) Source(key string) string { return r.src[key] }

// Merge folds layers in order into one configuration: later layers win per leaf,
// objects merge deeply, arrays and scalars replace. It is provenance tracking on
// top of MergeObjects' semantics; call it instead when you need Explain.
func Merge(layers ...Layer) (Resolved, error) {
	root := &val{k: kindObj, obj: &object{m: make(map[string]*val)}}
	src := make(map[string]string)
	for _, l := range layers {
		if len(bytes.TrimSpace(l.Data)) == 0 {
			continue
		}
		over, err := parseNode(l.Data)
		if err != nil {
			return Resolved{}, fmt.Errorf("config layer %s: %w", l.Name, err)
		} else if !over.isObject() {
			return Resolved{}, fmt.Errorf("config layer %s is not a JSON object", l.Name)
		}
		foldVal(root.obj, over.obj, "", l.Name, src)
	}
	return Resolved{root: root, src: src}, nil
}

// foldVal merges every key of over into dst at path, recording each leaf's source.
func foldVal(dst *object, over *object, path string, name string, src map[string]string) {
	for _, k := range over.keys {
		dv, ok := dst.m[k]
		if !ok {
			dst.keys = append(dst.keys, k)
		}
		childPath := join(path, k)
		ov := over.m[k]
		switch {
		case !ov.isObject():
			dst.m[k] = ov // scalar or array replaces wholesale
			src[childPath] = name
		case ok && dv.isObject():
			foldVal(dv.obj, ov.obj, childPath, name, src)
		default:
			fresh := &val{k: kindObj, obj: &object{m: make(map[string]*val)}}
			dst.m[k] = fresh // a scalar being replaced by an object adopts it
			foldVal(fresh.obj, ov.obj, childPath, name, src)
		}
	}
}

// splitPath splits a dotted key on its separator.
func splitPath(key string) []string {
	return strings.Split(key, ".")
}
