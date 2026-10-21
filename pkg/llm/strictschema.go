package llm

import (
	"encoding/json"
	"slices"

	"github.com/go-analyze/bulk"
)

// Schema keywords and type names the strict rewrite reads or emits.
const (
	strictKeyType       = "type"
	strictKeyProperties = "properties"
	strictKeyRequired   = "required"
	strictKeyItems      = "items"
	strictKeyAnyOf      = "anyOf"
	strictKeyAdditional = "additionalProperties"
	strictKeyEnum       = "enum"
	strictKeyConst      = "const"

	strictTypeObject = "object"
	strictTypeArray  = "array"
	strictTypeNull   = "null"
)

// unsupportedStrictKeys are JSON Schema keywords the anthropic strict subset
// rejects: unions, references and open-ended shapes it cannot sample from.
var unsupportedStrictKeys = []string{
	"$ref", "$defs", "definitions", "allOf", "oneOf",
	"patternProperties", "dependentSchemas", "dependencies",
	"unevaluatedProperties", "propertyNames", "contains", "prefixItems",
	"not", "if", "then", "else",
}

// strictNullAbsentKeys are schema keywords whose explicit JSON null spelling
// means absent, the same tolerance the MCP layer applies. const is excluded: a
// null const selects the null value instead of omitting the keyword.
var strictNullAbsentKeys = []string{
	strictKeyType, strictKeyProperties, strictKeyRequired, strictKeyItems,
	strictKeyAnyOf, strictKeyAdditional, strictKeyEnum,
}

// makeStrictAnthropicSchema rewrites a tool schema into the strict subset
// anthropic requires: no unions, every property required or null-wrapped and
// additionalProperties false. It reports false when a schema cannot be made
// strict, returning the input unchanged.
func makeStrictAnthropicSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root[strictKeyType] != strictTypeObject {
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

// strictAnthropicNode makes one schema node strict in place and reports whether
// it could be. False means the shape is not expressible strictly.
func strictAnthropicNode(s map[string]any) bool {
	strictDropNullKeywords(s)
	if slices.ContainsFunc(unsupportedStrictKeys, func(k string) bool { _, ok := s[k]; return ok }) {
		return false
	}
	// anyOf variants must be simple, never a structured object or array union
	if raw, ok := s[strictKeyAnyOf]; ok {
		list, isList := raw.([]any)
		if !isList || len(list) == 0 {
			return false
		}
		for _, v := range list {
			sub, isMap := v.(map[string]any)
			if !isMap || !strictAnthropicNode(sub) || strictAnthropicStructured(sub) {
				return false
			}
		}
	}
	switch it := s[strictKeyItems].(type) {
	case []any:
		return false // tuple schemas are unsupported
	case map[string]any:
		if !strictAnthropicNode(it) {
			return false
		}
	default:
		if _, present := s[strictKeyItems]; present {
			return false // a scalar item schema is not sampleable strictly
		}
	}

	isObjectSchema := strictHasType(s, strictTypeObject)
	if _, hasProps := s[strictKeyProperties]; hasProps && !isObjectSchema {
		return false // properties require type object
	}
	if !isObjectSchema {
		return true
	}
	if ap, ok := s[strictKeyAdditional]; ok && ap != false {
		return false
	}

	var props map[string]any
	if raw, ok := s[strictKeyProperties]; ok {
		m, isMap := raw.(map[string]any)
		if !isMap {
			return false
		}
		props = m
	} else {
		props = map[string]any{}
	}

	var requiredSet map[string]bool
	if raw, ok := s[strictKeyRequired]; ok {
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
		if requiredSet[name] || strictSchemaAllowsNull(sub) {
			continue
		}
		if strictAnthropicStructured(sub) {
			// the null wrap would emit the structured-member union the
			// provider refuses, so the tool cannot ship strict
			return false
		}
		// an optional non-nullable property becomes null-wrapped so required stays total
		props[name] = map[string]any{
			strictKeyAnyOf: []any{sub, map[string]any{strictKeyType: strictTypeNull}},
		}
	}

	// nil for an empty map marshals as null. Sort for deterministic bytes and
	// parity with how json.Marshal orders property keys, keeping strict tool
	// schemas cache-stable across requests
	required := bulk.MapKeysSlice(props)
	if required == nil {
		required = []string{}
	}
	slices.Sort(required)
	s[strictKeyRequired] = required
	s[strictKeyAdditional] = false
	return true
}

// strictDropNullKeywords removes null-valued keywords so tolerated spellings do
// not leak into the emitted strict schema.
func strictDropNullKeywords(s map[string]any) {
	for _, k := range slices.Concat(strictNullAbsentKeys, unsupportedStrictKeys) {
		if v, ok := s[k]; ok && v == nil {
			delete(s, k)
		}
	}
}

// strictHasType reports a node typed as name, matching both the single-name and
// type-array spellings of the same type.
func strictHasType(s map[string]any, name string) bool {
	switch t := s[strictKeyType].(type) {
	case string:
		return t == name
	case []any:
		return slices.ContainsFunc(t, func(x any) bool { return x == name })
	}
	return false
}

// strictAnthropicStructured reports a schema that is itself an object or array,
// which anthropic forbids inside anyOf unions.
func strictAnthropicStructured(s map[string]any) bool {
	if strictHasType(s, strictTypeObject) || strictHasType(s, strictTypeArray) {
		return true
	}
	if _, ok := s[strictKeyProperties]; ok {
		return true
	}
	_, hasItems := s[strictKeyItems]
	return hasItems
}

// strictSchemaAllowsNull reports a schema that already accepts null, so it does
// not need the anyOf null wrap.
func strictSchemaAllowsNull(s map[string]any) bool {
	if strictHasType(s, strictTypeNull) {
		return true
	}
	if c, ok := s[strictKeyConst]; ok && c == nil {
		return true
	}
	if e, ok := s[strictKeyEnum].([]any); ok {
		return slices.ContainsFunc(e, func(x any) bool { return x == nil })
	}
	return false
}
