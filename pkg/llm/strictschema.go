package llm

import (
	"encoding/json"
	"slices"

	"github.com/go-analyze/bulk"
)

// unsupportedStrictKeys are JSON Schema keywords the anthropic strict subset
// rejects: unions, references and open-ended shapes it cannot sample from.
var unsupportedStrictKeys = []string{
	"$ref", "$defs", "definitions", "allOf", "oneOf",
	"patternProperties", "dependentSchemas", "dependencies",
	"unevaluatedProperties", "propertyNames", "contains", "prefixItems",
	"not", "if", "then", "else",
}

// strictTypeObject is the JSON Schema type marker a strict tool root must have.
const strictTypeObject = "object"

// makeStrictAnthropicSchema rewrites a tool schema into the strict subset
// anthropic requires: no unions, every property required or null-wrapped and
// additionalProperties false. It reports false when a schema cannot be made strict.
func makeStrictAnthropicSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root["type"] != strictTypeObject {
		return raw, false // must be a top level object schema
	}
	if !strictAnthropicNode(root) {
		return raw, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return raw, false
	}
	return out, true
}

// strictAnthropicNode makes one schema node strict in place.
func strictAnthropicNode(s map[string]any) bool {
	for k := range s {
		if slices.Contains(unsupportedStrictKeys, k) {
			return false
		}
	}
	// anyOf variants must be simple, never a structured object or array union
	if raw, ok := s["anyOf"]; ok {
		list, isList := raw.([]any)
		if !isList || len(list) == 0 {
			return false
		}
		for _, v := range list {
			sub, isMap := v.(map[string]any)
			if !isMap || strictAnthropicStructured(sub) || !strictAnthropicNode(sub) {
				return false
			}
		}
	}
	switch it := s["items"].(type) {
	case []any:
		return false // tuple schemas are unsupported
	case map[string]any:
		if !strictAnthropicNode(it) {
			return false
		}
	default:
		if _, present := s["items"]; present {
			return false // a scalar item schema is not sampleable strictly
		}
	}

	isObjectSchema := s["type"] == strictTypeObject
	if _, hasProps := s["properties"]; hasProps && !isObjectSchema {
		return false // properties require type object
	}
	if !isObjectSchema {
		return true
	}
	if ap, ok := s["additionalProperties"]; ok && ap != false {
		return false
	}

	var props map[string]any
	if raw, ok := s["properties"]; ok {
		m, isMap := raw.(map[string]any)
		if !isMap {
			return false
		}
		props = m
	} else {
		props = map[string]any{}
	}

	var requiredSet map[string]bool
	if raw, ok := s["required"]; ok && raw != nil {
		list, isList := raw.([]any)
		if !isList {
			return false
		}
		requiredSet = make(map[string]bool, len(list))
		for _, r := range list {
			name, isStr := r.(string)
			if !isStr {
				return false
			}
			if _, known := props[name]; !known {
				return false // required names a property the schema does not have
			}
			requiredSet[name] = true
		}
	}

	for name, pv := range props {
		sub, isMap := pv.(map[string]any)
		if !isMap || !strictAnthropicNode(sub) {
			return false
		}
		// an optional non-nullable property becomes null-wrapped so required stays total
		if !requiredSet[name] && !strictSchemaAllowsNull(sub) {
			props[name] = map[string]any{
				"anyOf": []any{sub, map[string]any{"type": "null"}},
			}
		}
	}

	s["required"] = bulk.MapKeysSlice(props)
	s["additionalProperties"] = false
	return true
}

// strictAnthropicStructured reports a schema that is itself an object or array,
// which anthropic forbids inside anyOf unions.
func strictAnthropicStructured(s map[string]any) bool {
	switch t := s["type"].(type) {
	case string:
		if t == "object" || t == "array" {
			return true
		}
	case []any:
		if slices.ContainsFunc(t, func(x any) bool { return x == "object" || x == "array" }) {
			return true
		}
	}
	if _, ok := s["properties"]; ok {
		return true
	}
	_, hasItems := s["items"]
	return hasItems
}

// strictSchemaAllowsNull reports a schema that already accepts null, so it does
// not need the anyOf null wrap.
func strictSchemaAllowsNull(s map[string]any) bool {
	switch t := s["type"].(type) {
	case string:
		if t == "null" {
			return true
		}
	case []any:
		if slices.ContainsFunc(t, func(x any) bool { return x == nil }) {
			return true
		}
	}
	if c, ok := s["const"]; ok && c == nil {
		return true
	}
	if e, ok := s["enum"].([]any); ok {
		return slices.ContainsFunc(e, func(x any) bool { return x == nil })
	}
	return false
}
