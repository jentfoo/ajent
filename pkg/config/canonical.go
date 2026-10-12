package config

import (
	"reflect"
	"strings"
)

// keyRename is one object key rewritten to its canonical schema spelling.
type keyRename struct {
	from string // dotted path as written in the file
	to   string // the same path with every segment's declared name
}

// canonicalizeKeys rewrites object keys that match Settings only by letter case
// to each field's declared name, so merge, provenance and typed decode agree on
// one spelling. Keys nothing declares pass through untouched. Returns the
// rewritten JSON plus one rename per renamed path; malformed input is returned
// unchanged for the caller's own decode to report.
func canonicalizeKeys(data []byte) ([]byte, []keyRename) {
	root, err := parseNode(data)
	if err != nil || !root.isObject() {
		return data, nil
	}
	var renames []keyRename
	canonVal(root, reflect.TypeOf(Settings{}), "", "", &renames)
	if len(renames) == 0 {
		return data, nil
	}
	out, err := indentJSON(root.marshal())
	if err != nil {
		return data, nil
	}
	return out, renames
}

// canonVal renames keys inside v against t in place, appending each rename.
// from and to carry the path as written and its canonical spelling so far.
func canonVal(v *val, t reflect.Type, from, to string, renames *[]keyRename) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v == nil || t == nil || t.Kind() == reflect.Interface ||
		t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return // an Unmarshaler or interface consumes arbitrary JSON
	}

	switch {
	case v.isObject():
		canonObj(v.obj, t, from, to, renames)
	case v.k == kindArr && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array):
		for _, el := range v.arr {
			canonVal(el, t.Elem(), from+"[]", to+"[]", renames)
		}
	}
}

// canonObj rewrites o's keys against a struct or map type, rebuilding it so
// case-variant twins collapse: first-seen position, last value.
func canonObj(o *object, t reflect.Type, from, to string, renames *[]keyRename) {
	if t.Kind() == reflect.Map {
		for _, k := range o.keys { // map keys are user data, never renamed
			canonVal(o.m[k], t.Elem(), join(from, k), join(to, k), renames)
		}
		return
	} else if t.Kind() != reflect.Struct {
		return
	}

	fields := jsonFields(t)
	dst := &object{m: make(map[string]*val, len(o.m))}
	for _, k := range o.keys {
		child := o.m[k]
		canon := k
		if fd, ok := fields[strings.ToLower(k)]; ok {
			canon = fd.name
			if canon != k {
				*renames = append(*renames, keyRename{from: join(from, k), to: join(to, canon)})
			}
			canonVal(child, fd.typ, join(from, k), join(to, canon), renames)
		}
		if _, ok := dst.m[canon]; !ok {
			dst.keys = append(dst.keys, canon) // first-seen position wins on case twins
		}
		dst.m[canon] = child // a later twin's value overwrites, matching decode order
	}
	*o = *dst
}
