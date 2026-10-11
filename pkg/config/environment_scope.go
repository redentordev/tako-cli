package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/envexpand"
	"gopkg.in/yaml.v3"
)

// Environment-scoped expansion
//
// A command that works on one selected environment only needs the ${VAR}
// references of the parts of the config it can reach: every top-level
// section (project, registries, secretSources, notifications, volumes,
// builds, hooks, ...), the selected environment, the environments it
// extends, and the servers it can use. parseEnvironmentScopedDocument
// expands the document keeping unset references as literal ${VAR} text,
// checks that no unset reference is reachable from that scope, and then
// removes every other environment and every out-of-scope server that holds
// an unset reference before the document is decoded. Unexpanded text from
// another environment therefore never reaches the loaded Config.
//
// A document whose references are all set is decoded exactly as LoadConfig
// decodes it; scoping only applies when it would otherwise fail. Anything
// the scope cannot be computed for (a syntax error, an unknown or abstract
// selected environment) falls back to the strict missing-variable error.

// environmentScope is the part of a config document one environment uses.
type environmentScope struct {
	// environments holds the selected environment and its extends chain.
	environments map[string]bool
	// allServers is set when every declared server is in scope: the
	// environment selects servers by label or by default, or the servers
	// are enrolled platform nodes whose controller, builder, and edge roles
	// span environments.
	allServers bool
	servers    map[string]bool
}

func (s environmentScope) hasServer(name string) bool {
	return s.allServers || s.servers[name]
}

// scopeDocument is a decoded view of an expanded config document used to
// compute an environment scope and prune it.
type scopeDocument struct {
	isJSON bool
	keys   []string
	values map[string]any
	// yamlRoot is the alias-materialized, merge-flattened YAML root.
	yamlRoot *yaml.Node
	// jsonEntries are the ordered top-level JSON members.
	jsonEntries []jsonObjectEntry
}

// scopeEnvironment returns the environment whose scope bounds ${VAR}
// expansion, or "" when every reference is required. A preview is scoped to
// its base, from which the preview environment is synthesized.
func (o LoadOptions) scopeEnvironment() string {
	environment := strings.TrimSpace(o.Environment)
	if environment == "" {
		return ""
	}
	if o.Preview != nil {
		if base := strings.TrimSpace(o.Preview.Base); base != "" {
			return base
		}
	}
	return environment
}

func parseEnvironmentScopedDocument(data string, isJSON bool, environment string, opts LoadOptions) (*Config, error) {
	expanded, missing := expandConfigText(data, !isJSON, true)
	if len(missing) == 0 {
		return ParseConfigDocumentWithOptions([]byte(expanded), isJSON, opts)
	}
	missingErr := func(names []string) error {
		return fmt.Errorf("failed to expand config environment variables: %w", missingEnvironmentVariablesError(names))
	}

	doc, ok := newScopeDocument([]byte(expanded), isJSON)
	if !ok {
		return nil, missingErr(missing)
	}
	scope, ok := doc.scopeFor(environment)
	if !ok {
		return nil, missingErr(missing)
	}
	if opts.Preview != nil {
		// Keep a declared environment that collides with the preview
		// identity so the resolver reports the conflict instead of the
		// collision being pruned away silently.
		if name, err := NormalizePreviewName(opts.Preview.Name); err == nil {
			if _, declared := doc.environments()[PreviewIdentity(name)]; declared {
				scope.environments[PreviewIdentity(name)] = true
			}
		}
	}
	wanted := make(map[string]bool, len(missing))
	for _, name := range missing {
		wanted[name] = true
	}
	if needed := doc.reachableReferences(scope, wanted); len(needed) > 0 {
		return nil, missingErr(needed)
	}

	pruned, lines, err := doc.prune(scope, wanted)
	if err != nil {
		return nil, missingErr(missing)
	}
	cfg, err := ParseConfigDocumentWithOptions(pruned, isJSON, opts)
	if err != nil {
		if lines != nil {
			return nil, errors.New(remapYAMLLineReferences(err.Error(), lines))
		}
		return nil, err
	}
	cfg.deferredVariables = missing
	return cfg, nil
}

// DeferredEnvironmentVariables returns the sorted unset ${VAR} names that
// LoadConfigForEnvironment skipped because only other environments or
// servers outside the selected environment referenced them.
func (c *Config) DeferredEnvironmentVariables() []string {
	if c == nil || len(c.deferredVariables) == 0 {
		return nil
	}
	return append([]string(nil), c.deferredVariables...)
}

func newScopeDocument(data []byte, isJSON bool) (*scopeDocument, bool) {
	doc := &scopeDocument{isJSON: isJSON, values: map[string]any{}}
	if isJSON {
		entries, ok := parseJSONObject(data)
		if !ok {
			return nil, false
		}
		for _, entry := range entries {
			var value any
			if err := json.Unmarshal(entry.value, &value); err != nil {
				return nil, false
			}
			doc.keys = append(doc.keys, entry.key)
			doc.values[entry.key] = value
		}
		doc.jsonEntries = entries
		return doc, true
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, false
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, false
	}
	root, err := materializeYAMLAliases(document.Content[0])
	if err != nil {
		return nil, false
	}
	root = flattenYAMLMergeKeys(root)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if _, duplicate := doc.values[key]; duplicate {
			return nil, false
		}
		var value any
		if err := root.Content[i+1].Decode(&value); err != nil {
			return nil, false
		}
		doc.keys = append(doc.keys, key)
		doc.values[key] = value
	}
	doc.yamlRoot = root
	return doc, true
}

func (d *scopeDocument) environments() map[string]any {
	envs, _ := d.values["environments"].(map[string]any)
	return envs
}

func (d *scopeDocument) servers() map[string]any {
	servers, _ := d.values["servers"].(map[string]any)
	return servers
}

// scopeFor computes the scope of a declared environment that is concrete or
// is a preview base (an abstract environment with a preview: block, whose
// synthesized preview environment is the concrete one).
func (d *scopeDocument) scopeFor(environment string) (environmentScope, bool) {
	scope := environmentScope{environments: map[string]bool{}, servers: map[string]bool{}}
	envs := d.environments()
	selected, ok := envs[environment]
	if !ok {
		return scope, false
	}
	if body, _ := selected.(map[string]any); body != nil && body[environmentPreviewKey] == nil {
		if abstract, _ := body[environmentAbstractKey].(bool); abstract {
			return scope, false
		}
	}

	// The selected environment inherits every value of its extends chain,
	// so the whole chain is in scope. Cycles and unknown parents are left
	// to the resolver to report.
	for name := environment; name != "" && !scope.environments[name]; {
		body, ok := envs[name]
		if !ok {
			break
		}
		scope.environments[name] = true
		fields, _ := body.(map[string]any)
		parent, _ := fields[environmentExtendsKey].(string)
		name = parent
	}

	for _, server := range d.servers() {
		fields, _ := server.(map[string]any)
		if scalarString(fields["clusterId"]) != "" || scalarString(fields["nodeId"]) != "" {
			scope.allServers = true
		}
	}
	listsServers := false
	for name := range scope.environments {
		fields, _ := envs[name].(map[string]any)
		if fields == nil {
			continue
		}
		if fields["serverSelector"] != nil {
			scope.allServers = true
		}
		if names := stringList(fields["servers"]); len(names) > 0 {
			listsServers = true
			scope.addServers(names)
		}
		if proxy, _ := fields["proxy"].(map[string]any); proxy != nil {
			scope.addPlacementServers(proxy["placement"])
		}
		services, _ := fields["services"].(map[string]any)
		for _, service := range services {
			if serviceFields, _ := service.(map[string]any); serviceFields != nil {
				scope.addPlacementServers(serviceFields["placement"])
			}
		}
	}
	if !listsServers {
		// Without an explicit list the environment uses label selection or
		// every platform node; either may pick any server.
		scope.allServers = true
	}
	return scope, true
}

func (s *environmentScope) addServers(names []string) {
	for _, name := range names {
		s.servers[name] = true
	}
}

func (s *environmentScope) addPlacementServers(placement any) {
	if fields, _ := placement.(map[string]any); fields != nil {
		s.addServers(stringList(fields["servers"]))
	}
}

func scalarString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func stringList(value any) []string {
	items, _ := value.([]any)
	var names []string
	for _, item := range items {
		if name := scalarString(item); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// reachableReferences returns the sorted wanted names referenced anywhere
// the scope can reach: every top-level section except environments, servers,
// and x- fragments (whose content reaches its users through YAML aliases,
// already materialized), plus the in-scope environments and servers.
func (d *scopeDocument) reachableReferences(scope environmentScope, wanted map[string]bool) []string {
	found := map[string]bool{}
	for _, key := range d.keys {
		if strings.HasPrefix(key, extensionKeyPrefix) {
			continue
		}
		collectReferences(key, wanted, found)
		value := d.values[key]
		switch key {
		case "environments":
			envs := d.environments()
			if envs == nil {
				collectReferences(value, wanted, found)
				continue
			}
			for name := range scope.environments {
				collectReferences(name, wanted, found)
				collectReferences(envs[name], wanted, found)
			}
		case "servers":
			servers := d.servers()
			if servers == nil {
				collectReferences(value, wanted, found)
				continue
			}
			for name, server := range servers {
				if scope.hasServer(name) {
					collectReferences(name, wanted, found)
					collectReferences(server, wanted, found)
				}
			}
		default:
			collectReferences(value, wanted, found)
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func collectReferences(value any, wanted map[string]bool, found map[string]bool) {
	switch typed := value.(type) {
	case string:
		if !strings.Contains(typed, "${") {
			return
		}
		_, refs := envexpand.Braced(typed, func(string) (string, bool) { return "", false })
		for _, ref := range refs {
			if wanted[ref] {
				found[ref] = true
			}
		}
	case map[string]any:
		for key, item := range typed {
			collectReferences(key, wanted, found)
			collectReferences(item, wanted, found)
		}
	case map[any]any:
		for key, item := range typed {
			collectReferences(key, wanted, found)
			collectReferences(item, wanted, found)
		}
	case []any:
		for _, item := range typed {
			collectReferences(item, wanted, found)
		}
	}
}

// prunedServers returns the out-of-scope servers that reference a wanted
// (unset) name; they cannot be expanded and are removed.
func (d *scopeDocument) prunedServers(scope environmentScope, wanted map[string]bool) map[string]bool {
	pruned := map[string]bool{}
	for name, server := range d.servers() {
		if scope.hasServer(name) {
			continue
		}
		found := map[string]bool{}
		collectReferences(name, wanted, found)
		collectReferences(server, wanted, found)
		if len(found) > 0 {
			pruned[name] = true
		}
	}
	return pruned
}

// prune re-encodes the document with only the in-scope environments and
// without the servers that hold unset references. For YAML it also returns
// the map from re-encoded lines to original lines for error messages.
func (d *scopeDocument) prune(scope environmentScope, wanted map[string]bool) ([]byte, map[int]int, error) {
	droppedServers := d.prunedServers(scope, wanted)
	keepEnvironment := func(name string) bool { return scope.environments[name] }
	keepServer := func(name string) bool { return !droppedServers[name] }

	if d.isJSON {
		var buf bytes.Buffer
		writeJSONObject(&buf, len(d.jsonEntries), func(i int) (string, func()) {
			entry := d.jsonEntries[i]
			return entry.key, func() {
				switch entry.key {
				case "environments":
					writeFilteredJSONObject(&buf, entry.value, keepEnvironment)
				case "servers":
					writeFilteredJSONObject(&buf, entry.value, keepServer)
				default:
					buf.Write(bytes.TrimSpace(entry.value))
				}
			}
		})
		buf.WriteByte('\n')
		return buf.Bytes(), nil, nil
	}

	root := *d.yamlRoot
	root.Content = make([]*yaml.Node, 0, len(d.yamlRoot.Content))
	for i := 0; i+1 < len(d.yamlRoot.Content); i += 2 {
		key, value := d.yamlRoot.Content[i], d.yamlRoot.Content[i+1]
		switch key.Value {
		case "environments":
			value = filteredYAMLMapping(value, keepEnvironment)
		case "servers":
			value = filteredYAMLMapping(value, keepServer)
		}
		root.Content = append(root.Content, key, value)
	}
	encoded, err := yaml.Marshal(&root)
	if err != nil {
		return nil, nil, err
	}
	return encoded, yamlLineMap(&root, encoded), nil
}

func filteredYAMLMapping(node *yaml.Node, keep func(string) bool) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return node
	}
	filtered := *node
	filtered.Content = nil
	for i := 0; i+1 < len(node.Content); i += 2 {
		if keep(node.Content[i].Value) {
			filtered.Content = append(filtered.Content, node.Content[i], node.Content[i+1])
		}
	}
	return &filtered
}

func writeFilteredJSONObject(buf *bytes.Buffer, raw json.RawMessage, keep func(string) bool) {
	entries, ok := parseJSONObject(raw)
	if !ok {
		buf.Write(bytes.TrimSpace(raw))
		return
	}
	var kept []jsonObjectEntry
	for _, entry := range entries {
		if keep(entry.key) {
			kept = append(kept, entry)
		}
	}
	writeJSONObject(buf, len(kept), func(i int) (string, func()) {
		return kept[i].key, func() { buf.Write(bytes.TrimSpace(kept[i].value)) }
	})
}
