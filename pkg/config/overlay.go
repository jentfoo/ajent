package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// OverlayJSON returns over with every base key schema does not declare grafted
// back in, so a save through typed structs keeps configuration this build cannot
// represent. Keys the schema declares are governed by over: present when it
// carries them, absent when it omits them. Objects merge per key, array elements
// pair by a shared non-empty "id" and otherwise replace wholesale, and any other
// shape change takes over's value. The result is indented at two spaces.
func OverlayJSON(base, over []byte, schema any) ([]byte, error) {
	root, err := parseNode(over)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(base)) == 0 {
		return indentJSON(root.marshal())
	}
	prev, err := parseNode(base)
	if err != nil {
		return nil, err // refuse to save over contents that cannot be inspected
	}
	graft(root, prev, reflect.TypeOf(schema))
	return indentJSON(root.marshal())
}

// graft lays the parts of prev dst cannot express onto dst. t is the schema
// type at this position, and everything it declares belongs to dst alone.
func graft(dst, prev *val, t reflect.Type) {
	if dst == nil || prev == nil || opaque(t) {
		return
	}
	switch {
	case dst.isObject() && prev.isObject():
		graftObject(dst.obj, prev.obj, t)
	case dst.k == kindArr && prev.k == kindArr:
		graftArray(dst.arr, prev.arr, t)
	}
}

// graftObject copies prev's unrecognized keys onto dst and recurses into shared
// children the schema declares. Unrecognized subtrees copy whole: their interior
// is unknown by construction.
func graftObject(dst, prev *object, t reflect.Type) {
	schema := objectSchemaFor(t)
	for _, k := range prev.keys {
		if _, ok := schema.child(k); ok {
			continue // governed by over: present or absent as the struct says
		}
		if _, exists := dst.m[k]; !exists {
			dst.keys = append(dst.keys, k)
			dst.m[k] = prev.m[k]
		}
	}
	for _, k := range dst.keys {
		ft, ok := schema.child(k)
		if !ok {
			continue // grafted whole above, or untyped and already verbatim
		}
		if pv, ok := prev.m[k]; ok {
			graft(dst.m[k], pv, ft)
		}
	}
}

// graftArray pairs elements by a shared non-empty "id", so an element's
// unrecognized keys survive a re-save. Unpaired elements are governed by over.
func graftArray(dst, prev []*val, t reflect.Type) {
	var elem reflect.Type
	if t != nil && !opaque(t) {
		if e := deref(t); e.Kind() == reflect.Slice || e.Kind() == reflect.Array {
			elem = e.Elem()
		}
	}
	pending := make(map[string][]*val, len(prev))
	for _, p := range prev {
		if id := elementID(p); id != "" {
			pending[id] = append(pending[id], p)
		}
	}
	for _, d := range dst {
		id := elementID(d)
		if len(pending[id]) == 0 {
			continue
		}
		p := pending[id][0]
		pending[id] = pending[id][1:]
		graft(d, p, elem)
	}
}

// objectSchema resolves an object position's declared keys to their schema types.
type objectSchema struct {
	fields map[string]fieldDecl // a struct's fields by lower JSON name, or nil
	elem   reflect.Type         // a map declares every key with one element type
}

// objectSchemaFor reads the schema at an object position: a struct's fields or a
// map's element type. Any other kind, like a scalar, declares nothing.
func objectSchemaFor(t reflect.Type) objectSchema {
	if t == nil {
		return objectSchema{}
	}
	switch e := deref(t); e.Kind() {
	case reflect.Struct:
		return objectSchema{fields: jsonFields(e)}
	case reflect.Map:
		return objectSchema{elem: e.Elem()}
	}
	return objectSchema{}
}

// child returns the schema type a key declares, if any.
func (s objectSchema) child(key string) (reflect.Type, bool) {
	if s.elem != nil {
		return s.elem, true
	}
	fd, ok := s.fields[strings.ToLower(key)]
	if !ok {
		return nil, false
	}
	return fd.typ, true
}

// elementID returns an object's non-empty "id" scalar, or "" when it has none.
func elementID(v *val) string {
	if !v.isObject() {
		return ""
	}
	n, ok := v.obj.m["id"]
	if !ok || n.k != kindScalar {
		return ""
	}
	var id string
	if err := json.Unmarshal(n.raw, &id); err != nil {
		return "" // a non-string id pairs nothing
	}
	return id
}

// opaque reports whether t consumes arbitrary JSON, so nothing inside it can be
// told from known: interfaces and Unmarshaler implementations such as
// json.RawMessage.
func opaque(t reflect.Type) bool {
	if t == nil {
		return false
	}
	e := deref(t)
	return e.Kind() == reflect.Interface ||
		e.Implements(unmarshalerType) ||
		reflect.PointerTo(e).Implements(unmarshalerType)
}
