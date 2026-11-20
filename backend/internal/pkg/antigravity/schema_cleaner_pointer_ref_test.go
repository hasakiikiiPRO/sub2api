package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// MCP/zod 生成的 schema 用 JSON Pointer 复用子结构（无 $defs），旧逻辑会原样留下 $ref → 上游 400。
func TestCleanJSONSchema_ResolvesJSONPointerRefs(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"type":"object",
		"properties":{
			"destination":{"type":"object","properties":{
				"parent":{"anyOf":[{"type":"array","items":{"type":"string"}},{"type":"string"}]},
				"other":{"$ref":"#/properties/destination/properties/parent/anyOf/1"},
				"self":{"$ref":"#/properties/destination"},
				"missing":{"$ref":"#/properties/nope"},
				"remote":{"$ref":"https://example.com/x.json"}
			}}
		}
	}`), &schema))

	cleaned := CleanJSONSchema(schema)
	raw, err := json.Marshal(cleaned)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "$ref")

	props := cleaned["properties"].(map[string]any)["destination"].(map[string]any)["properties"].(map[string]any)
	require.Equal(t, "string", props["other"].(map[string]any)["type"])
}
