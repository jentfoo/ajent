package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// EnvLayer returns the layer bound from AJENT_* variables, one per scalar key
// the schema declares. Var names derive from the dotted path: reasoning.level maps
// to AJENT_REASONING_LEVEL. An unparseable number or bool is a warning, not an error.
func EnvLayer(env func(string) string) (Layer, []string) {
	var warns []string
	data := []byte("{}")
	for _, p := range scalarLeaves(reflect.TypeOf(Settings{}), "") {
		name := "AJENT_" + strings.ToUpper(strings.ReplaceAll(p.path, ".", "_"))
		v := env(name)
		if v == "" {
			continue
		}
		jv, err := encodeScalar(p.kind, v)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s=%q: %v", name, v, err))
			continue
		}
		data, _ = SetKey(data, p.path, jv)
	}
	return Layer{Name: "env", Data: data}, warns
}

// leafPath is one scalar schema key and its Go kind.
type leafPath struct {
	path string
	kind reflect.Kind
}

var rawMessageType = reflect.TypeOf((*json.RawMessage)(nil)).Elem()

// scalarLeaves returns the scalar dotted paths Settings declares, recursing into
// nested structs and skipping slices, maps and provider/model subtrees (which are
// opaque to this package).
func scalarLeaves(t reflect.Type, prefix string) []leafPath {
	var out []leafPath
	for name, ft := range jsonFields(t) {
		p := join(prefix, name)
		if derefKind(ft) == reflect.Struct && !isRawMessage(ft) {
			out = append(out, scalarLeaves(deref(ft), p)...)
			continue
		}
		k := derefKind(ft)
		switch k {
		case reflect.String, reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Float32, reflect.Float64:
			out = append(out, leafPath{p, k})
		}
	}
	return out
}

// isRawMessage reports whether t is encoding/json.RawMessage.
func isRawMessage(t reflect.Type) bool { return t == rawMessageType }

func derefKind(t reflect.Type) reflect.Kind { return deref(t).Kind() }

// encodeScalar converts an env string into the JSON value for kind k.
func encodeScalar(k reflect.Kind, s string) (any, error) {
	switch k {
	case reflect.String:
		return s, nil
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("want true or false: %w", err)
		}
		return b, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("want an integer: %w", err)
		}
		return n, nil
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("want a number: %w", err)
		}
		return f, nil
	default:
		return s, nil
	}
}
