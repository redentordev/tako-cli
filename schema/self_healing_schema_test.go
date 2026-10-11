package schema

import "testing"

func TestTakoSchemaDescribesSelfHealing(t *testing.T) {
	schema := loadTakoSchema(t)
	environment := schemaPath(t, schema, "properties", "environments", "additionalProperties", "properties")
	service := schemaPath(t, environment, "services", "additionalProperties", "properties")
	for _, ref := range []map[string]any{schemaPath(t, environment, "selfHealing"), schemaPath(t, service, "selfHealing")} {
		if ref["$ref"] != "#/definitions/selfHealing" {
			t.Fatalf("selfHealing ref = %#v", ref)
		}
	}
	block := schemaPath(t, schema, "definitions", "selfHealing")
	if block["type"] != "object" || block["additionalProperties"] != false {
		t.Fatalf("selfHealing = %#v, want closed object", block)
	}
	properties := schemaPath(t, block, "properties")
	for key, wantType := range map[string]string{
		"enabled": "boolean", "restartUnhealthy": "boolean", "startExited": "boolean",
		"unhealthyThreshold": "integer", "maxRestarts": "integer", "window": "string",
	} {
		if got := schemaPath(t, properties, key)["type"]; got != wantType {
			t.Fatalf("selfHealing.%s type = %#v, want %s", key, got, wantType)
		}
	}
	for _, key := range []string{"unhealthyThreshold", "maxRestarts"} {
		field := schemaPath(t, properties, key)
		if field["minimum"] != float64(1) || field["maximum"] != float64(20) {
			t.Fatalf("selfHealing.%s bounds = %#v", key, field)
		}
	}
}
