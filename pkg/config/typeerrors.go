package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

var jsonNumberType = reflect.TypeOf(json.Number(""))

// TypeErrors returns one warning per dotted path whose JSON value cannot decode
// into the type of v, following encoding/json's rules: null fits every target,
// RawMessage and interfaces accept arbitrary JSON, and a ,string field wants its
// scalar inside quotes. Use it to warn about wrong-typed values without failing
// the load.
func TypeErrors(data []byte, v any) ([]string, error) {
	var node any
	if err := decodeJSON(data, &node); err != nil {
		return nil, err
	}
	var out []string
	walkTypes(node, reflect.TypeOf(v), "", "", &out)
	return out, nil
}

// walkTypes descends node alongside t, appending a warning for every path whose
// value cannot unmarshal into t.
func walkTypes(node any, t reflect.Type, opts string, path string, out *[]string) {
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
		walkTypesObject(n, t, opts, path, out)
	case []any:
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			for _, v := range n {
				walkTypes(v, t.Elem(), "", path+"[]", out)
			}
		} else {
			*out = append(*out, typeMismatch(path, node, t, opts))
		}
	default: // string, bool, number or null
		if !scalarFits(node, t, opts) {
			*out = append(*out, typeMismatch(path, node, t, opts))
		}
	}
}

// walkTypesObject checks an object's keys against t. Keys t does not declare
// belong to UnknownKeys and are skipped here.
func walkTypesObject(n map[string]any, t reflect.Type, opts string, path string, out *[]string) {
	switch t.Kind() {
	case reflect.Struct:
		fields := jsonFields(t)
		for k, v := range n {
			fd, ok := fields[strings.ToLower(k)] // encoding/json matches case insensitively
			if !ok {
				continue
			}
			walkTypes(v, fd.typ, fd.opts, join(path, k), out)
		}
	case reflect.Map:
		for k, v := range n {
			walkTypes(v, t.Elem(), "", join(path, k), out)
		}
	default:
		*out = append(*out, typeMismatch(path, n, t, opts))
	}
}

// scalarFits reports whether a JSON scalar decodes into t. opts are the struct
// tag options beyond the name, where ",string" wants the value inside quotes.
func scalarFits(node any, t reflect.Type, opts string) bool {
	if node == nil {
		return true // null is a no-op for every target type
	}
	if t == jsonNumberType {
		_, ok := node.(json.Number)
		return ok
	}
	strOpt := hasStringOpt(opts)
	switch n := node.(type) {
	case string:
		if strOpt {
			return quotedFits(n, t)
		}
		return t.Kind() == reflect.String
	case bool:
		return !strOpt && t.Kind() == reflect.Bool
	case json.Number:
		return !strOpt && numberFits(string(n), t)
	}
	return false
}

// quotedFits reports whether a ,string field's content decodes into t unquoted.
func quotedFits(s string, t reflect.Type) bool {
	var inner any
	if err := decodeJSON([]byte(s), &inner); err != nil {
		return false // the content is not a JSON value at all
	}
	switch v := inner.(type) {
	case nil:
		return true // a quoted null is still a no-op
	case bool:
		return t.Kind() == reflect.Bool
	case json.Number:
		return numberFits(string(v), t)
	case string: // the doubly-quoted form a ,string string field wants
		return t.Kind() == reflect.String
	default: // containers never decode through ,string
		return false
	}
}

// numberFits reports whether a JSON number literal decodes into t, range checked
// against the target's size like encoding/json does.
func numberFits(lit string, t reflect.Type) bool {
	rv := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(lit, 10, 64)
		return err == nil && !rv.OverflowInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr:
		n, err := strconv.ParseUint(lit, 10, 64)
		return err == nil && !rv.OverflowUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(lit, 64)
		return err == nil && !rv.OverflowFloat(f)
	default:
		return false
	}
}

// typeMismatch renders one path whose value does not fit t as a warning line.
func typeMismatch(path string, got any, t reflect.Type, opts string) string {
	return fmt.Sprintf("wrong type for key %q: got %s, want %s", path, gotName(got), wantName(t, opts))
}

// gotName renders the JSON kind of a decoded value. null never reaches here.
func gotName(node any) string {
	switch node.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	default:
		return "a string"
	}
}

// wantName renders the JSON shape t decodes from.
func wantName(t reflect.Type, opts string) string {
	strOpt := hasStringOpt(opts)
	switch t.Kind() {
	case reflect.String:
		if strOpt {
			return "a quoted string"
		}
		return "a string"
	case reflect.Bool:
		if strOpt {
			return "a quoted boolean"
		}
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if strOpt {
			return "a quoted integer"
		}
		return "an integer"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr:
		if strOpt {
			return "a quoted unsigned integer"
		}
		return "an unsigned integer"
	case reflect.Float32, reflect.Float64:
		if strOpt {
			return "a quoted number"
		}
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	default: // struct and map
		return "an object"
	}
}

// hasStringOpt reports whether tag options carry the ,string decode option.
func hasStringOpt(opts string) bool {
	return slices.Contains(strings.Split(opts, ","), "string")
}
