package schema

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestTakoSchemaIsValidJSON(t *testing.T) {
	loadTakoSchema(t)
}

func TestTakoSchemaAlignsWithSupportedTakodModel(t *testing.T) {
	schema := loadTakoSchema(t)

	assertStringEnum(t, schemaPath(t, schema, "properties", "runtime", "properties", "mode"), []string{config.RuntimeModeTakod})
	assertBoolConst(t, schemaPath(t, schema, "properties", "mesh", "properties", "enabled"), true)
	assertStringEnum(t, schemaPath(t, schema, "properties", "state", "properties", "backend"), []string{config.StateBackendReplicated})
	assertStringEnum(t, schemaPath(t, schema, "properties", "state", "properties", "deployConsistency"), []string{config.StateDeployConsistencyLease})
	assertStringEnum(t, schemaPath(t, schema, "properties", "state", "properties", "onUnreachableNode"), []string{config.StateUnreachableBlock})
	assertBoolEnum(t, schemaPath(t, schema, "properties", "state", "properties", "remoteCacheEnabled"), []bool{true})
	assertStringEnum(t, schemaPath(t, schema, "properties", "environments", "additionalProperties", "properties", "services", "additionalProperties", "properties", "loadBalancer", "properties", "strategy"), []string{"round_robin", "sticky"})
}

func loadTakoSchema(t *testing.T) map[string]any {
	t.Helper()

	data, err := os.ReadFile("tako.schema.json")
	if err != nil {
		t.Fatalf("failed to read schema: %v", err)
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return schema
}

func schemaPath(t *testing.T, value any, keys ...string) map[string]any {
	t.Helper()

	current := value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("schema path %v reached non-object %#v", keys, current)
		}
		current, ok = object[key]
		if !ok {
			t.Fatalf("schema path %v missing key %q", keys, key)
		}
	}

	object, ok := current.(map[string]any)
	if !ok {
		t.Fatalf("schema path %v reached non-object %#v", keys, current)
	}
	return object
}

func assertStringEnum(t *testing.T, field map[string]any, want []string) {
	t.Helper()

	gotRaw, ok := field["enum"].([]any)
	if !ok {
		t.Fatalf("schema field missing string enum: %#v", field)
	}
	got := make([]string, 0, len(gotRaw))
	for _, value := range gotRaw {
		stringValue, ok := value.(string)
		if !ok {
			t.Fatalf("schema enum contains non-string value %#v in %#v", value, gotRaw)
		}
		got = append(got, stringValue)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema enum = %#v, want %#v", got, want)
	}
}

func assertBoolEnum(t *testing.T, field map[string]any, want []bool) {
	t.Helper()

	gotRaw, ok := field["enum"].([]any)
	if !ok {
		t.Fatalf("schema field missing bool enum: %#v", field)
	}
	got := make([]bool, 0, len(gotRaw))
	for _, value := range gotRaw {
		boolValue, ok := value.(bool)
		if !ok {
			t.Fatalf("schema enum contains non-bool value %#v in %#v", value, gotRaw)
		}
		got = append(got, boolValue)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema enum = %#v, want %#v", got, want)
	}
}

func assertBoolConst(t *testing.T, field map[string]any, want bool) {
	t.Helper()

	got, ok := field["const"].(bool)
	if !ok {
		t.Fatalf("schema field missing bool const: %#v", field)
	}
	if got != want {
		t.Fatalf("schema const = %v, want %v", got, want)
	}
}
