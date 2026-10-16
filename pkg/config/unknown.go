package config

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/go-analyze/bulk"
)

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// UnknownKeys returns the dotted JSON paths present in data but not declared by
// the type of v. Use it to warn about typos without failing the load.
func UnknownKeys(data []byte, v any) ([]string, error) {
	var node any
	if err := decodeJSON(data, &node); err != nil {
		return nil, err
	}
	var out []string
	walkUnknown(node, reflect.TypeOf(v), "", &out)
	return out, nil
}

// walkUnknown descends node alongside t, appending paths t does not declare.
func walkUnknown(node any, t reflect.Type, path string, out *[]string) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// an Unmarshaler or interface/pointer-to-interface consumes arbitrary JSON
	if t == nil || t.Kind() == reflect.Interface ||
		t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return
	}

	switch n := node.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			fields := jsonFields(t)
			for k, v := range n {
				fd, ok := fields[strings.ToLower(k)] // encoding/json matches case insensitively
				if !ok {
					*out = append(*out, join(path, k))
					continue
				}
				walkUnknown(v, fd.typ, join(path, k), out)
			}
		case reflect.Map:
			for k, v := range n {
				walkUnknown(v, t.Elem(), join(path, k), out)
			}
		}
	case []any:
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			for _, v := range n {
				walkUnknown(v, t.Elem(), path+"[]", out)
			}
		}
	}
}

// jsonFields maps the lower cased JSON names t declares to their full field
// declarations, flattening embedded structs the way encoding/json does. Names are
// lowercased for case-insensitive lookup against decoded keys.
func jsonFields(t reflect.Type) map[string]fieldDecl {
	return bulk.SliceToIndexBy(func(fd fieldDecl) string { return strings.ToLower(fd.name) }, declaredFields(t))
}

// fieldDecl is one struct field with its JSON name as the schema declares it.
type fieldDecl struct {
	name string // original-case JSON tag or Go field name
	opts string // tag options after the name, e.g. "string", "omitempty"
	typ  reflect.Type
}

// declaredFields returns t's fields in declaration order, flattening anonymous
// embedded structs the way encoding/json does. Names keep their original case so
// callers can build schema paths that match config files and env bindings.
func declaredFields(t reflect.Type) []fieldDecl {
	var out []fieldDecl
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		} else if name == "" && f.Anonymous && deref(f.Type).Kind() == reflect.Struct {
			// embedded struct fields are promoted even when the type is unexported
			out = append(out, declaredFields(deref(f.Type))...)
			continue
		} else if !f.IsExported() {
			continue
		} else if name == "" {
			name = f.Name
		}
		out = append(out, fieldDecl{name: name, opts: opts, typ: f.Type})
	}
	return out
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
