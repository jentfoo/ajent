package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
)

// maxSchemaDepth bounds recursion; discovery runs pre-turn, so a pathological
// schema must fail fast rather than burn CPU.
const maxSchemaDepth = 64

// propKeyRe is the key pattern providers police on parameter objects
// (Anthropic: 'Property keys should match pattern ^[a-zA-Z0-9_.-]{1,64}').
// Dots are legal here, unlike tool names.
var propKeyRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// combinatorKeys are the keywords whose value is a non-empty array of schemas.
var combinatorKeys = []string{"anyOf", "oneOf", "allOf"}

// schemaMapKeys are the keywords whose value is an object of named schemas.
// $defs/definitions stay unchecked: $ref is never resolved, so a def's body is
// dead metadata no provider reads.
var schemaMapKeys = []string{"properties", "patternProperties", "dependentSchemas"}

// schemaKeys are the keywords whose value is a single schema.
var schemaKeys = []string{"additionalProperties", "propertyNames", "contains", "not", "if", "then", "else"}

// schemaDefect reports the first structural defect in a remote tool schema, or
// "" when sound. Best-effort walk, not a JSON-Schema interpreter: a node is
// rejected only when a provider could not encode it.
func schemaDefect(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return "not a JSON object"
	}
	top, ok := v.(map[string]any)
	if !ok {
		return "not a JSON object"
	}
	if t, ok := top["type"].(string); !ok || t != "object" { // the MCP spec pins the root type
		return `type must be "object"`
	}
	// the root walks the same keywords as any nested node, combinators included
	if reason := shapesDefect("", top, 0); reason != "" {
		return reason
	}
	return nodeDefect("", top, 0)
}

// nodeDefect walks one object schema node's schema-valued keywords. Explicit
// nulls count as absent: generators emit them where a keyword is optional, and
// no provider polices their presence.
func nodeDefect(path string, node map[string]any, depth int) string {
	for _, key := range schemaMapKeys {
		v, ok := node[key]
		if !ok || v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return schemaPath(path, key) + " is not an object"
		}
		names := bulk.MapKeysSlice(m)
		slices.Sort(names) // deterministic first-defect order for the warning
		for _, name := range names {
			path := schemaPath(path, key) + "." + name
			if key == "properties" && !propKeyRe.MatchString(name) {
				// property keys ride into the request body verbatim and are
				// provider-validated, unlike patternProperties regex keys
				return path + " is not a valid property name"
			}
			if reason := schemaAt(path, m[name], depth+1); reason != "" {
				return reason
			}
		}
	}
	if v, ok := node["required"]; ok && v != nil {
		if _, isBool := v.(bool); isBool {
			// draft-03 boolean form; providers ignore it, so judge legacy dialects leniently
		} else {
			names, ok := v.([]any)
			if !ok {
				return schemaPath(path, "required") + " is not an array of property names"
			}
			for _, n := range names {
				if _, ok := n.(string); !ok {
					return schemaPath(path, "required") + " is not an array of property names"
				}
			}
		}
	}
	for _, key := range []string{"items", "prefixItems"} {
		v, ok := node[key]
		if !ok || v == nil {
			continue
		}
		if reason := itemsDefect(schemaPath(path, key), v, depth+1); reason != "" {
			return reason
		}
	}
	for _, key := range schemaKeys {
		if v, ok := node[key]; ok && v != nil {
			if reason := schemaAt(schemaPath(path, key), v, depth+1); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// itemsDefect validates an items or prefixItems value: one schema, or draft-07
// tuple form.
func itemsDefect(path string, v any, depth int) string {
	if list, ok := v.([]any); ok {
		for i, item := range list {
			if reason := schemaAt(fmt.Sprintf("%s[%d]", path, i), item, depth); reason != "" {
				return reason
			}
		}
		return ""
	}
	return schemaAt(path, v, depth)
}

// schemaAt validates one schema position: an object walked in full, or a
// boolean schema (always/never applies). Boolean forms are spec-mandated and
// tolerated by the providers; flagged for a real-request fixture if one ever
// rejects them.
func schemaAt(path string, v any, depth int) string {
	if depth > maxSchemaDepth {
		return path + " nested too deeply"
	}
	switch n := v.(type) {
	case bool:
		return ""
	case map[string]any:
		if reason := shapesDefect(path, n, depth); reason != "" {
			return reason
		}
		if raw, ok := n["type"]; ok && raw != nil {
			if reason := typeDefect(path, raw); reason != "" {
				return reason
			}
		}
		return nodeDefect(path, n, depth)
	default:
		return path + " is not an object"
	}
}

// shapesDefect validates branching and constraint keywords: each combinator an
// array of schemas, enum an array, $ref a non-empty string. Arrays may be
// empty: unsatisfiable, but encodable and not policed on the wire.
func shapesDefect(path string, node map[string]any, depth int) string {
	for _, key := range combinatorKeys {
		v, ok := node[key]
		if !ok || v == nil {
			continue
		}
		branches, ok := v.([]any)
		if !ok {
			return fmt.Sprintf("%s.%s is not an array", path, key)
		}
		for i, branch := range branches {
			if reason := schemaAt(fmt.Sprintf("%s.%s[%d]", path, key, i), branch, depth+1); reason != "" {
				return reason
			}
		}
	}
	if v, ok := node["enum"]; ok && v != nil {
		if _, ok := v.([]any); !ok {
			return path + ".enum is not an array"
		}
	}
	if v, ok := node["$ref"]; ok && v != nil {
		ref, ok := v.(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return path + ".$ref is not a non-empty string"
		}
	}
	return ""
}

// typeDefect validates a type keyword: one known name or a non-empty array of
// them.
func typeDefect(path string, v any) string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return path + " has an empty type name"
		}
		if knownType(t) {
			return ""
		}
		return fmt.Sprintf("%s has unknown type %q", path, t)
	case []any:
		if len(t) == 0 {
			return path + " has a malformed type"
		}
		for _, n := range t {
			name, ok := n.(string)
			if !ok {
				return path + " has a malformed type"
			}
			if !knownType(name) {
				return fmt.Sprintf("%s has unknown type %q", path, name)
			}
		}
		return ""
	default:
		return path + " has a malformed type"
	}
}

// knownType reports whether name is a provider-accepted JSON Schema type.
func knownType(name string) bool {
	switch name {
	case "string", "number", "integer", "boolean", "object", "array", "null":
		return true
	}
	return false
}

// schemaPath renders a node's location for defect messages; the root has none.
func schemaPath(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}
