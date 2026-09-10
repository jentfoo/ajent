package strutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// DecodeArgs unmarshals raw into v, returning an error written for the model
// rather than Go's decoder: it names the offending field (by its JSON tag) and
// the expected/actual JSON kind, so a malformed call is self-correcting. A lone
// value where an array field is declared heals into a one-element array. Callers
// prepend their own context (e.g. "bad args:").
func DecodeArgs(raw json.RawMessage, v any) error {
	root := reflect.TypeOf(v)
	err := json.Unmarshal(raw, v)
	if err == nil {
		return nil
	}
	if healed, ok := healSingletons(raw, root); ok {
		if err = json.Unmarshal(healed, v); err == nil {
			return nil
		}
	}
	return errors.New(argError(err, root))
}

// argError renders err as a message for the model. root is the declared
// arguments type.
func argError(err error, root reflect.Type) string {
	var ue *json.UnmarshalTypeError
	if errors.As(err, &ue) {
		want := kindName(ue.Type)
		if ue.Field == "" {
			// slice roots carry no field name: element and whole-value failures differ
			if elem, ok := rootElem(root); ok {
				if elem == ue.Type {
					return fmt.Sprintf("each element must be %s, but was given a JSON %s", want, ue.Value)
				}
				return fmt.Sprintf("expected %s, but was given a JSON %s", want, ue.Value)
			}
			return fmt.Sprintf("arguments must be %s, but was given a JSON %s", want, ue.Value)
		}
		// element failures are reported against the field's own name
		if elem, ok := sliceElem(root, ue.Field); ok && elem == ue.Type {
			return fmt.Sprintf("each item of %s must be %s, but was given a JSON %s", ue.Field, want, ue.Value)
		}
		return fmt.Sprintf("%s must be %s, but was given a JSON %s", ue.Field, want, ue.Value)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return "arguments are not valid JSON"
	}
	return "arguments are malformed"
}

// rootElem returns root's element type when root dereferences to a slice or array.
func rootElem(root reflect.Type) (reflect.Type, bool) {
	for root.Kind() == reflect.Pointer {
		root = root.Elem()
	}
	if root.Kind() == reflect.Slice || root.Kind() == reflect.Array {
		return root.Elem(), true
	}
	return nil, false
}

// sliceElem returns the element type of field on root when it names a
// top-level slice or array, matched by JSON tag with a case-insensitive
// fallback. Nested paths are skipped: Go reports leaf failures against the
// leaf type, where element phrasing never applies.
func sliceElem(root reflect.Type, field string) (reflect.Type, bool) {
	if strings.Contains(field, ".") {
		return nil, false
	}
	for root.Kind() == reflect.Pointer {
		root = root.Elem()
	}
	if root.Kind() != reflect.Struct {
		return nil, false
	}
	for i := 0; i < root.NumField(); i++ {
		f := root.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" {
			name = f.Name
		}
		if name != field && !strings.EqualFold(f.Name, field) {
			continue
		}
		t := f.Type
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			return t.Elem(), true
		}
		return nil, false
	}
	return nil, false
}

// healSingletons wraps single values sent for declared array fields into
// one-element arrays when their shape fits the element type.
func healSingletons(raw json.RawMessage, root reflect.Type) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw, false
	}
	var changed bool
	for name, val := range fields {
		if len(val) == 0 || val[0] == '[' || val[0] == 'n' {
			continue
		}
		elem, ok := sliceElem(root, name)
		if !ok || !elemShape(val[0], elem) {
			continue
		}
		fields[name] = json.RawMessage("[" + string(val) + "]")
		changed = true
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw, false
	}
	return out, true
}

// elemShape reports whether b can open a value of elem's kind.
func elemShape(b byte, elem reflect.Type) bool {
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	switch elem.Kind() {
	case reflect.Struct, reflect.Map:
		return b == '{'
	case reflect.String:
		return b == '"'
	case reflect.Bool:
		return b == 't' || b == 'f'
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return b == '-' || (b >= '0' && b <= '9')
	default:
		return false
	}
}

// kindName renders the Go type t as the JSON shape it expects.
func kindName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Interface {
			return "a JSON array"
		}
		return "a JSON array of " + plural(kindName(t.Elem()))
	case reflect.Map, reflect.Struct:
		return "a JSON object"
	default:
		return "an object"
	}
}

// plural pluralizes a kindName result.
func plural(kind string) string {
	kind = strings.TrimPrefix(kind, "a ")
	kind = strings.TrimPrefix(kind, "an ")
	kind = strings.TrimPrefix(kind, "JSON ")
	return kind + "s"
}
