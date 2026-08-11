package tools

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type schemaFixture struct {
	Name    string   `json:"name"`
	Count   int      `json:"count,omitempty"`
	Enabled bool     `json:"enabled"`
	Mode    string   `json:"mode,omitempty" enum:"a,b,c"`
	Tags    []string `json:"tags,omitempty"`
}

type nestedFixture struct {
	Name  string       `json:"name"`
	Inner innerFixture `json:"inner"`
}

type innerFixture struct {
	Value int `json:"value"`
}

func TestSchemaOf(t *testing.T) {
	t.Parallel()

	schema := SchemaOf[schemaFixture]()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(schema, &obj))

	assert.Equal(t, "object", obj["type"])
	assert.False(t, obj["additionalProperties"].(bool))

	props := obj["properties"].(map[string]any)
	nameProp := props["name"].(map[string]any)
	assert.Equal(t, "string", nameProp["type"])

	countProp := props["count"].(map[string]any)
	assert.Equal(t, "integer", countProp["type"])

	enabledProp := props["enabled"].(map[string]any)
	assert.Equal(t, "boolean", enabledProp["type"])

	modeProp := props["mode"].(map[string]any)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, modeProp["enum"])

	tagsProp := props["tags"].(map[string]any)
	assert.Equal(t, "array", tagsProp["type"])
	items := tagsProp["items"].(map[string]any)
	assert.Equal(t, "string", items["type"])

	required := obj["required"].([]any)
	assert.ElementsMatch(t, []string{"name", "enabled"}, required) // omitempty fields are optional
}

func TestSchemaNestedStruct(t *testing.T) {
	t.Parallel()

	schema := SchemaOf[nestedFixture]()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(schema, &obj))

	props := obj["properties"].(map[string]any)
	inner := props["inner"].(map[string]any)
	assert.Equal(t, "object", inner["type"])
	innerProps := inner["properties"].(map[string]any)
	assert.Contains(t, innerProps, "value")
}

func TestSchemaPanicsOnUnsupportedKind(t *testing.T) {
	t.Parallel()

	type bad struct {
		M map[string]string `json:"m"`
	}
	assert.Panics(t, func() { SchemaOf[bad]() })
}
