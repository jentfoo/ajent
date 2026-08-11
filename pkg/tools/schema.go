// Package tools provides the built-in coding-agent toolset: read, write, edit
// and bash enabled by default plus find and grep registered off. It implements
// pkg/agent.Tool directly and never imports pkg/tui, keeping the front end free.
package tools

import (
	"encoding/json"
	"reflect"
	"strings"
)

// SchemaOf returns the JSON Schema for T, derived from its json, desc and enum
// struct tags. It panics on an unsupported field kind, which surfaces at tool
// registration rather than at model call time.
func SchemaOf[T any]() json.RawMessage {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() != reflect.Struct {
		panic("tools: SchemaOf requires a struct, got " + t.String())
	}
	schema := objectSchema{
		Type:                 "object",
		Properties:           propertiesFor(t),
		AdditionalProperties: false,
	}
	for i := 0; i < t.NumField(); i++ {
		if _, required, _ := fieldMeta(t.Field(i)); required {
			schema.Required = append(schema.Required, fieldJSON(t.Field(i)))
		}
	}
	return marshalSchema(schema)
}

// propNode is one JSON Schema property.
type propNode struct {
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	Enum        []string       `json:"enum,omitempty"`
	Items       *propNode      `json:"items,omitempty"`
	Properties  map[string]any `json:"properties,omitempty"`
	Required    []string       `json:"required,omitempty"`
}

// objectSchema is the top-level tool parameters shape.
type objectSchema struct {
	Type                 string         `json:"type"`
	Properties           map[string]any `json:"properties"`
	Required             []string       `json:"required,omitempty"`
	AdditionalProperties bool           `json:"additionalProperties"`
}

// propertiesFor builds every property node for a struct type. Nested structs
// become object nodes and slices of scalar/struct become array nodes.
func propertiesFor(t reflect.Type) map[string]any {
	out := make(map[string]any)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported field, skip
			continue
		}
		name, _, node := fieldMeta(f)
		if name == "" {
			continue
		}
		out[name] = node
	}
	return out
}

// fieldMeta returns a property's json name, whether it is required (no
// omitempty), and its schema node built from the struct tags.
func fieldMeta(f reflect.StructField) (name string, required bool, node propNode) {
	name = fieldJSON(f)
	if name == "" || name == "-" {
		return "", false, propNode{}
	}
	node = nodeFor(f.Type)
	node.Description = f.Tag.Get("desc")
	if enum := f.Tag.Get("enum"); enum != "" {
		node.Enum = splitList(enum)
	}
	return name, !hasOmitempty(f), node
}

// fieldJSON returns the json tag name without options.
func fieldJSON(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if i := strings.IndexByte(tag, ','); i >= 0 {
		return tag[:i]
	}
	return tag
}

func hasOmitempty(f reflect.StructField) bool {
	for _, part := range splitList(f.Tag.Get("json")) {
		if part == "omitempty" {
			return true
		}
	}
	return false
}

// nodeFor builds the schema node for a field type, recursing into slices and
// nested structs. It panics on an unsupported kind.
func nodeFor(t reflect.Type) propNode {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return propNode{Type: "string"}
	case reflect.Bool:
		return propNode{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return propNode{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return propNode{Type: "number"}
	case reflect.Slice:
		elem := nodeFor(t.Elem())
		return propNode{Type: "array", Items: &elem}
	case reflect.Struct:
		node := propNode{
			Type:       "object",
			Properties: propertiesFor(t),
		}
		for i := 0; i < t.NumField(); i++ {
			if _, required, _ := fieldMeta(t.Field(i)); required {
				node.Required = append(node.Required, fieldJSON(t.Field(i)))
			}
		}
		return node
	default:
		panic("tools: unsupported schema kind " + t.Kind().String() + " for " + t.String())
	}
}

func marshalSchema(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("tools: cannot marshal schema: " + err.Error())
	}
	return raw
}

// splitList splits a comma-separated tag value into trimmed parts.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
