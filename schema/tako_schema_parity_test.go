package schema

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

// schemaParityAllowlist records intentional differences between the Go config
// types and schema/tako.schema.json, keyed by config path. Map keys appear as
// "*" and array items as "[]". Every entry needs a reason.
var schemaParityAllowlist = map[string]string{
	// ServiceConfig.Build is a string in Go; the structured object form is
	// decoded by ServiceConfig.UnmarshalYAML/UnmarshalJSON through the
	// unexported structuredServiceBuild (pkg/config/service_build.go).
	"environments.*.services.*.build.context": "structured build form decoded by structuredServiceBuild",
	"environments.*.services.*.build.args":    "structured build form decoded by structuredServiceBuild",
	"environments.*.services.*.build.target":  "structured build form decoded by structuredServiceBuild",
}

// schemaParityLeafTypes are config types with custom unmarshalers whose
// accepted shapes the walk does not descend into.
var schemaParityLeafTypes = map[reflect.Type]string{
	reflect.TypeOf(config.StringOrList{}): "string or argv list",
}

type schemaParityReport struct {
	missingInSchema []string
	staleInSchema   []string
	tagMismatches   []string
}

func TestTakoSchemaMatchesConfigTypes(t *testing.T) {
	schema := loadTakoSchema(t)
	report := &schemaParityReport{}
	used := map[string]bool{}
	walkSchemaParity(schema, reflect.TypeOf(config.Config{}), schema, "", report, used)

	for _, list := range [][]string{report.missingInSchema, report.staleInSchema, report.tagMismatches} {
		sort.Strings(list)
	}
	for _, path := range report.missingInSchema {
		t.Errorf("config key %s is accepted by pkg/config but not declared in schema/tako.schema.json (add it or allowlist it with a reason)", path)
	}
	for _, path := range report.staleInSchema {
		t.Errorf("schema property %s has no matching pkg/config field (remove it or allowlist it with a reason)", path)
	}
	for _, mismatch := range report.tagMismatches {
		t.Errorf("yaml/json tag mismatch: %s", mismatch)
	}
	for path, reason := range schemaParityAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("allowlist entry %s has no reason", path)
		}
		if !used[path] {
			t.Errorf("allowlist entry %s is unused; remove it", path)
		}
	}
}

func TestTakoSchemaDeclaresPreviouslyMissingAndDeprecatedKeys(t *testing.T) {
	schema := loadTakoSchema(t)
	environment := schemaPath(t, schema, "properties", "environments", "additionalProperties", "properties")
	service := schemaPath(t, environment, "services", "additionalProperties", "properties")
	schemaPath(t, service, "dockerfile")
	assertStringEnum(t, schemaPath(t, service, "monitoring", "properties", "checkType"), []string{"http", "container"})
	schemaPath(t, environment, "serverSelector", "properties", "labels")
	schemaPath(t, environment, "serverSelector", "properties", "any")

	deployment := schemaPath(t, schema, "properties", "deployment", "properties")
	for name, node := range map[string]map[string]any{
		"deployment.strategy":   schemaPath(t, deployment, "strategy"),
		"deployment.parallel":   schemaPath(t, deployment, "parallel"),
		"deployment.cache":      schemaPath(t, deployment, "cache"),
		"environments.*.labels": schemaPath(t, environment, "labels"),
	} {
		if node["deprecated"] != true {
			t.Fatalf("%s must be marked deprecated: %#v", name, node)
		}
		description, _ := node["description"].(string)
		if !strings.Contains(description, config.DeprecatedKeysRemovalRelease) {
			t.Fatalf("%s description must name the removal release: %q", name, description)
		}
	}
	if _, deprecated := schemaPath(t, deployment, "build")["deprecated"]; deprecated {
		t.Fatal("deployment.build must not be deprecated")
	}
}

func TestSchemaParityDetectsMissingAndStaleKeys(t *testing.T) {
	type nested struct {
		Kept    string `yaml:"kept" json:"kept"`
		Missing string `yaml:"missing" json:"missing"`
		Skipped string `yaml:"-" json:"-"`
	}
	type root struct {
		Items map[string]nested `yaml:"items" json:"items"`
		Bad   string            `yaml:"bad" json:"other"`
	}
	schema := map[string]any{
		"properties": map[string]any{
			"bad": map[string]any{"type": "string"},
			"items": map[string]any{
				"additionalProperties": map[string]any{
					"oneOf": []any{
						map[string]any{"properties": map[string]any{"kept": map[string]any{}}},
						map[string]any{"properties": map[string]any{"stale": map[string]any{}}},
					},
				},
			},
		},
	}
	report := &schemaParityReport{}
	walkSchemaParity(schema, reflect.TypeOf(root{}), schema, "", report, map[string]bool{})
	if !reflect.DeepEqual(report.missingInSchema, []string{"items.*.missing"}) {
		t.Fatalf("missing = %#v", report.missingInSchema)
	}
	if !reflect.DeepEqual(report.staleInSchema, []string{"items.*.stale"}) {
		t.Fatalf("stale = %#v", report.staleInSchema)
	}
	if len(report.tagMismatches) != 1 || !strings.Contains(report.tagMismatches[0], "bad") {
		t.Fatalf("tag mismatches = %#v", report.tagMismatches)
	}
}

func walkSchemaParity(root map[string]any, typ reflect.Type, node map[string]any, path string, report *schemaParityReport, used map[string]bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	node = resolveSchemaRef(root, node)
	if _, leaf := schemaParityLeafTypes[typ]; leaf {
		return
	}

	switch typ.Kind() {
	case reflect.Struct:
		fields := structYAMLFields(typ, path, report)
		properties := schemaProperties(root, node)
		for _, name := range sortedKeys(fields) {
			childPath := joinSchemaPath(path, name)
			child, ok := properties[name]
			if !ok {
				if _, allowed := schemaParityAllowlist[childPath]; allowed {
					used[childPath] = true
					continue
				}
				report.missingInSchema = append(report.missingInSchema, childPath)
				continue
			}
			walkSchemaParity(root, fields[name], child, childPath, report, used)
		}
		for _, name := range sortedKeys(properties) {
			if _, ok := fields[name]; ok {
				continue
			}
			reportStale(joinSchemaPath(path, name), report, used)
		}
	case reflect.Map:
		if child, ok := node["additionalProperties"].(map[string]any); ok {
			walkSchemaParity(root, typ.Elem(), child, path+".*", report, used)
		}
		// Documented keys of a free-form map (for example ACME credentials)
		// are not struct fields, so explicit properties are allowed here.
	case reflect.Slice, reflect.Array:
		if child, ok := node["items"].(map[string]any); ok {
			walkSchemaParity(root, typ.Elem(), child, path+"[]", report, used)
		}
	default:
		// Scalar in Go: any object properties the schema declares under it
		// have no decoder unless allowlisted.
		for _, name := range sortedKeys(schemaProperties(root, node)) {
			reportStale(joinSchemaPath(path, name), report, used)
		}
	}
}

func reportStale(path string, report *schemaParityReport, used map[string]bool) {
	if _, allowed := schemaParityAllowlist[path]; allowed {
		used[path] = true
		return
	}
	report.staleInSchema = append(report.staleInSchema, path)
}

func structYAMLFields(typ reflect.Type, path string, report *schemaParityReport) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		yamlName := tagName(field.Tag.Get("yaml"))
		jsonName := tagName(field.Tag.Get("json"))
		if yamlName != jsonName {
			report.tagMismatches = append(report.tagMismatches, joinSchemaPath(path, field.Name)+": yaml "+quoteTag(yamlName)+" != json "+quoteTag(jsonName))
		}
		if yamlName == "-" || yamlName == "" {
			continue
		}
		fields[yamlName] = field.Type
	}
	return fields
}

func tagName(tag string) string {
	name, _, _ := strings.Cut(tag, ",")
	return name
}

func quoteTag(name string) string {
	if name == "" {
		return "(none)"
	}
	return "\"" + name + "\""
}

// schemaProperties unions properties declared directly on node and in its
// oneOf/anyOf/allOf branches and then/else conditionals, following $ref.
func schemaProperties(root, node map[string]any) map[string]map[string]any {
	properties := map[string]map[string]any{}
	var collect func(map[string]any)
	collect = func(current map[string]any) {
		current = resolveSchemaRef(root, current)
		if props, ok := current["properties"].(map[string]any); ok {
			for name, raw := range props {
				child, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if existing, ok := properties[name]; ok {
					properties[name] = mergeSchemaNodes(existing, child)
				} else {
					properties[name] = child
				}
			}
		}
		for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
			if branches, ok := current[keyword].([]any); ok {
				for _, raw := range branches {
					if branch, ok := raw.(map[string]any); ok {
						collect(branch)
					}
				}
			}
		}
		for _, keyword := range []string{"then", "else"} {
			if branch, ok := current[keyword].(map[string]any); ok {
				collect(branch)
			}
		}
	}
	collect(node)
	return properties
}

// mergeSchemaNodes combines two declarations of the same property (for
// example a base definition and a conditional refinement) so the walk sees
// the union of their nested properties.
func mergeSchemaNodes(a, b map[string]any) map[string]any {
	return map[string]any{"allOf": []any{a, b}}
}

func resolveSchemaRef(root, node map[string]any) map[string]any {
	for {
		ref, ok := node["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return node
		}
		var current any = root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := current.(map[string]any)
			if !ok {
				return node
			}
			current = m[part]
		}
		resolved, ok := current.(map[string]any)
		if !ok {
			return node
		}
		node = resolved
	}
}

func joinSchemaPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
