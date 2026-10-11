package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxEnvironmentExtendsDepth bounds an extends chain: an environment may sit
// at most this many extends hops below its root ancestor.
const maxEnvironmentExtendsDepth = 8

const (
	environmentExtendsKey  = "extends"
	environmentAbstractKey = "abstract"
	extensionKeyPrefix     = "x-"
)

// environmentResolution records how environments were resolved from the
// declared document. It is attached to a Config only when the document used
// extends, abstract or top-level x- keys; plain configs keep a nil value.
type environmentResolution struct {
	// declared lists every declared environment name in declaration order.
	declared []string
	abstract map[string]bool
	// parents maps an inheriting environment to the environment it extends.
	parents map[string]string
	// chains maps every environment to its ancestors, nearest first.
	chains map[string][]string
	// provenance maps environment -> dotted value path -> the declared
	// environment that supplied that value.
	provenance map[string]map[string]string
	// encodedLines maps a line of the re-encoded YAML document to the line
	// in the original file it came from, for error messages.
	encodedLines map[int]int
}

// configDecodeError marks a resolver failure that is a strict decode error
// (unknown field or wrong type) rather than an inheritance error, so callers
// can report it with the same "failed to parse" prefix as the plain decoder.
type configDecodeError struct{ err error }

func (e *configDecodeError) Error() string { return e.err.Error() }
func (e *configDecodeError) Unwrap() error { return e.err }

// envNodeKind classifies a document node for merging.
type envNodeKind int

const (
	envNodeNull envNodeKind = iota
	envNodeScalar
	envNodeSequence
	envNodeMapping
)

// envNode is a format-neutral view of one environment document used by the
// merge. Mappings keep key order; scalars and sequences are opaque leaves
// that keep the original YAML node or JSON bytes so re-encoding is lossless.
type envNode struct {
	kind     envNodeKind
	keys     []string
	values   []*envNode
	keyNodes []*yaml.Node // YAML only: original key nodes
	yaml     *yaml.Node   // YAML only: leaf node, or mapping attributes
	raw      json.RawMessage
}

func newEnvMapping() *envNode {
	return &envNode{kind: envNodeMapping}
}

func (n *envNode) appendEntry(key string, keyNode *yaml.Node, value *envNode) {
	n.keys = append(n.keys, key)
	n.keyNodes = append(n.keyNodes, keyNode)
	n.values = append(n.values, value)
}

// declaredEnvironment is one environment as written in the config, with its
// extends/abstract keys split off.
type declaredEnvironment struct {
	name       string
	body       *envNode
	extends    string
	hasExtends bool
	abstract   bool
}

// mergeEnvironmentNodes merges child over parent following the inheritance
// rules: mappings merge by key, scalars and sequences replace, a kind change
// replaces, and an explicit null removes the inherited key. Provenance in
// prov is updated for every path the child touches.
func mergeEnvironmentNodes(parent, child *envNode, path, owner string, prov map[string]string) *envNode {
	out := newEnvMapping()
	out.yaml = child.yaml
	if out.yaml == nil {
		out.yaml = parent.yaml
	}
	index := make(map[string]int, len(parent.keys))
	for i, key := range parent.keys {
		index[key] = len(out.keys)
		out.appendEntry(key, parent.keyNodes[i], parent.values[i])
	}
	removed := map[int]bool{}
	for i, key := range child.keys {
		value := child.values[i]
		subPath := joinValuePath(path, key)
		j, inherited := index[key]
		switch {
		case value.kind == envNodeNull:
			dropValueProvenance(prov, subPath)
			if inherited {
				removed[j] = true
			}
		case inherited && !removed[j] && value.kind == envNodeMapping && out.values[j].kind == envNodeMapping:
			out.values[j] = mergeEnvironmentNodes(out.values[j], value, subPath, owner, prov)
			out.keyNodes[j] = child.keyNodes[i]
		case inherited:
			dropValueProvenance(prov, subPath)
			recordValueProvenance(value, subPath, owner, prov)
			out.values[j] = value
			out.keyNodes[j] = child.keyNodes[i]
			delete(removed, j)
		default:
			recordValueProvenance(value, subPath, owner, prov)
			index[key] = len(out.keys)
			out.appendEntry(key, child.keyNodes[i], value)
		}
	}
	if len(removed) == 0 {
		return out
	}
	filtered := newEnvMapping()
	filtered.yaml = out.yaml
	for i, key := range out.keys {
		if !removed[i] {
			filtered.appendEntry(key, out.keyNodes[i], out.values[i])
		}
	}
	return filtered
}

func joinValuePath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func recordValueProvenance(node *envNode, path, owner string, prov map[string]string) {
	if node.kind == envNodeMapping && len(node.keys) > 0 {
		for i, key := range node.keys {
			recordValueProvenance(node.values[i], joinValuePath(path, key), owner, prov)
		}
		return
	}
	if path != "" {
		prov[path] = owner
	}
}

func dropValueProvenance(prov map[string]string, path string) {
	prefix := path + "."
	for key := range prov {
		if key == path || strings.HasPrefix(key, prefix) {
			delete(prov, key)
		}
	}
}

// resolveEnvironmentInheritance resolves every declared environment and
// returns the resolved bodies keyed by name together with the metadata.
func resolveEnvironmentInheritance(envs []declaredEnvironment) (map[string]*envNode, *environmentResolution, error) {
	byName := make(map[string]*declaredEnvironment, len(envs))
	meta := &environmentResolution{
		abstract:   map[string]bool{},
		parents:    map[string]string{},
		chains:     map[string][]string{},
		provenance: map[string]map[string]string{},
	}
	for i := range envs {
		env := &envs[i]
		byName[env.name] = env
		meta.declared = append(meta.declared, env.name)
		if env.abstract {
			meta.abstract[env.name] = true
			if !isValidRuntimeIdentifier(env.name) {
				return nil, nil, fmt.Errorf("environment name '%s' is invalid: must start with a lowercase letter, contain only lowercase letters, numbers, hyphens, and underscores, and be 1-63 characters long", env.name)
			}
		}
		if env.hasExtends {
			meta.parents[env.name] = env.extends
		}
	}

	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	resolved := map[string]*envNode{}
	var resolve func(name string, stack []string) error
	resolve = func(name string, stack []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			start := 0
			for i, entry := range stack {
				if entry == name {
					start = i
					break
				}
			}
			cycle := append(append([]string{}, stack[start:]...), name)
			return fmt.Errorf("environment extends cycle: %s", strings.Join(cycle, " -> "))
		}
		env := byName[name]
		if !env.hasExtends {
			prov := map[string]string{}
			recordValueProvenance(env.body, "", name, prov)
			resolved[name] = env.body
			meta.provenance[name] = prov
			state[name] = done
			return nil
		}
		if _, ok := byName[env.extends]; !ok {
			return fmt.Errorf("environment %s extends unknown environment %q", name, env.extends)
		}
		state[name] = visiting
		if err := resolve(env.extends, append(stack, name)); err != nil {
			return err
		}
		chain := append([]string{env.extends}, meta.chains[env.extends]...)
		if len(chain) > maxEnvironmentExtendsDepth {
			return fmt.Errorf("environment %s extends chain exceeds the maximum depth of %d: %s", name, maxEnvironmentExtendsDepth, strings.Join(append([]string{name}, chain...), " -> "))
		}
		prov := make(map[string]string, len(meta.provenance[env.extends]))
		for path, owner := range meta.provenance[env.extends] {
			prov[path] = owner
		}
		resolved[name] = mergeEnvironmentNodes(resolved[env.extends], env.body, "", name, prov)
		meta.chains[name] = chain
		meta.provenance[name] = prov
		state[name] = done
		return nil
	}

	names := append([]string{}, meta.declared...)
	sort.Strings(names)
	for _, name := range names {
		if err := resolve(name, nil); err != nil {
			return nil, nil, err
		}
	}
	if err := checkResolvedBuildContexts(names, byName, resolved, meta); err != nil {
		return nil, nil, err
	}
	return resolved, meta, nil
}

// checkResolvedBuildContexts requires a structured build context in every
// deployable inheriting environment once the merge is done. The declared
// environments were decoded with that check relaxed so a child can override
// only part of an inherited build.
func checkResolvedBuildContexts(names []string, byName map[string]*declaredEnvironment, resolved map[string]*envNode, meta *environmentResolution) error {
	for _, name := range names {
		env := byName[name]
		if env.abstract || !env.hasExtends {
			continue
		}
		if missing := missingBuildContexts(resolved[name]); len(missing) > 0 {
			return fmt.Errorf("environment %s: service %s: invalid structured build: context is required (environment %s extends %s)", name, missing[0], name, strings.Join(meta.chains[name], " -> "))
		}
	}
	return nil
}

// resolveConfigDocument applies top-level x- key stripping and environment
// inheritance to an expanded config document. A document that uses none of
// these features is returned unchanged (same bytes, nil metadata) so the
// strict decoder reports exactly what it reported before inheritance existed.
func resolveConfigDocument(data []byte, isJSON bool) ([]byte, *environmentResolution, error) {
	if isJSON {
		return resolveJSONConfigDocument(data)
	}
	return resolveYAMLConfigDocument(data)
}

// ---- YAML adapter ----

func resolveYAMLConfigDocument(data []byte) ([]byte, *environmentResolution, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return data, nil, nil // the strict decoder reports syntax errors
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return data, nil, nil
	}
	if !yamlDocumentNeedsResolution(document.Content[0]) {
		return data, nil, nil
	}

	root, err := materializeYAMLAliases(document.Content[0])
	if err != nil {
		return nil, nil, &configDecodeError{err: err}
	}
	root = flattenYAMLMergeKeys(root)

	stripped := *root
	stripped.Content = nil
	var environments *yaml.Node
	environmentsIndex := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if strings.HasPrefix(key.Value, extensionKeyPrefix) {
			continue
		}
		if key.Value == "environments" && environmentsIndex < 0 {
			environments = root.Content[i+1]
			environmentsIndex = len(stripped.Content) + 1
		}
		stripped.Content = append(stripped.Content, key, root.Content[i+1])
	}

	var meta *environmentResolution
	if environments != nil && environments.Kind == yaml.MappingNode {
		declared := make([]declaredEnvironment, 0, len(environments.Content)/2)
		keyNodes := map[string]*yaml.Node{}
		for i := 0; i+1 < len(environments.Content); i += 2 {
			nameNode := environments.Content[i]
			env, err := declareYAMLEnvironment(nameNode.Value, environments.Content[i+1])
			if err != nil {
				return nil, nil, err
			}
			keyNodes[env.name] = nameNode
			declared = append(declared, env)
		}
		resolved, resolution, err := resolveEnvironmentInheritance(declared)
		if err != nil {
			return nil, nil, err
		}
		meta = resolution
		rebuilt := *environments
		rebuilt.Content = nil
		for _, env := range declared {
			if env.abstract {
				continue
			}
			rebuilt.Content = append(rebuilt.Content, keyNodes[env.name], envNodeToYAML(resolved[env.name]))
		}
		stripped.Content[environmentsIndex] = &rebuilt
	}
	if meta == nil {
		meta = &environmentResolution{abstract: map[string]bool{}, parents: map[string]string{}, chains: map[string][]string{}, provenance: map[string]map[string]string{}}
	}

	encoded, err := yaml.Marshal(&stripped)
	if err != nil {
		return nil, nil, &configDecodeError{err: err}
	}
	meta.encodedLines = yamlLineMap(&stripped, encoded)
	return encoded, meta, nil
}

func declareYAMLEnvironment(name string, node *yaml.Node) (declaredEnvironment, error) {
	env := declaredEnvironment{name: name}
	body := node
	if node.Kind == yaml.MappingNode {
		copyNode := *node
		copyNode.Content = nil
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			switch key.Value {
			case environmentExtendsKey:
				if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!str" || strings.TrimSpace(value.Value) == "" {
					return env, fmt.Errorf("environment %s: line %d: extends must be the name of another environment", name, value.Line)
				}
				env.extends = value.Value
				env.hasExtends = true
				continue
			case environmentAbstractKey:
				var abstract bool
				if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!bool" || value.Decode(&abstract) != nil {
					return env, fmt.Errorf("environment %s: line %d: abstract must be true or false", name, value.Line)
				}
				env.abstract = abstract
				continue
			}
			copyNode.Content = append(copyNode.Content, key, value)
		}
		body = &copyNode
	}

	if body.Kind == yaml.MappingNode {
		env.body = yamlToEnvNode(body)
	} else {
		env.body = newEnvMapping()
	}

	// Strictly decode the declared environment on its own so unknown or
	// mistyped fields are reported against the environment that declared
	// them, not one that inherited them. A partial structured build in an
	// inheriting or abstract environment may get its context from the
	// parent; checkResolvedBuildContexts requires it after the merge.
	decodeNode := body
	if env.relaxesBuildContext() && body.Kind == yaml.MappingNode {
		decodeNode = envNodeToYAML(withPlaceholderBuildContexts(env.body, false))
	}
	var decoded EnvironmentConfig
	if err := decodeYAMLNodeStrict(decodeNode, &decoded); err != nil {
		return env, &configDecodeError{err: fmt.Errorf("environment %s: %w", name, err)}
	}
	return env, nil
}

// relaxesBuildContext reports whether a declared environment may leave a
// structured build's context to an inherited value.
func (e declaredEnvironment) relaxesBuildContext() bool {
	return e.hasExtends || e.abstract
}

// withPlaceholderBuildContexts returns a copy of an environment body in which
// every structured services.<name>.build mapping without a context gets a
// placeholder context, so the strict per-environment decode accepts a partial
// build override. The copy is only decoded, never merged or deployed.
func withPlaceholderBuildContexts(body *envNode, isJSON bool) *envNode {
	servicesIndex := envNodeIndex(body, "services")
	if servicesIndex < 0 || body.values[servicesIndex].kind != envNodeMapping {
		return body
	}
	services := body.values[servicesIndex]
	patchedServices := *services
	patchedServices.values = append([]*envNode{}, services.values...)
	changed := false
	for i, service := range services.values {
		buildIndex := envNodeIndex(service, "build")
		if buildIndex < 0 || service.values[buildIndex].kind != envNodeMapping {
			continue
		}
		build := service.values[buildIndex]
		// A null context removes the inherited one, so it is checked
		// after the merge like an absent context.
		contextIndex := envNodeIndex(build, "context")
		if contextIndex >= 0 && build.values[contextIndex].kind != envNodeNull {
			continue
		}
		placeholder := &envNode{kind: envNodeScalar, raw: json.RawMessage(`"."`)}
		if !isJSON {
			placeholder.yaml = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "."}
		}
		patchedBuild := *build
		patchedBuild.keys = append([]string{}, build.keys...)
		patchedBuild.keyNodes = append([]*yaml.Node{}, build.keyNodes...)
		patchedBuild.values = append([]*envNode{}, build.values...)
		if contextIndex >= 0 {
			patchedBuild.values[contextIndex] = placeholder
		} else {
			patchedBuild.keys = append(patchedBuild.keys, "context")
			patchedBuild.keyNodes = append(patchedBuild.keyNodes, nil)
			patchedBuild.values = append(patchedBuild.values, placeholder)
		}
		patchedService := *service
		patchedService.values = append([]*envNode{}, service.values...)
		patchedService.values[buildIndex] = &patchedBuild
		patchedServices.values[i] = &patchedService
		changed = true
	}
	if !changed {
		return body
	}
	patched := *body
	patched.values = append([]*envNode{}, body.values...)
	patched.values[servicesIndex] = &patchedServices
	return &patched
}

func envNodeIndex(node *envNode, key string) int {
	if node == nil || node.kind != envNodeMapping {
		return -1
	}
	for i, candidate := range node.keys {
		if candidate == key {
			return i
		}
	}
	return -1
}

// envNodeString returns a scalar leaf as a string, or ok false when it is not
// a string scalar.
func envNodeString(node *envNode) (string, bool) {
	if node == nil || node.kind != envNodeScalar {
		return "", false
	}
	if node.yaml != nil {
		return node.yaml.Value, node.yaml.ShortTag() == "!!str"
	}
	var value string
	if err := json.Unmarshal(node.raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// missingBuildContexts returns, sorted, the services of a resolved
// environment whose structured build has no non-empty context.
func missingBuildContexts(body *envNode) []string {
	servicesIndex := envNodeIndex(body, "services")
	if servicesIndex < 0 {
		return nil
	}
	services := body.values[servicesIndex]
	var missing []string
	for i, service := range services.values {
		buildIndex := envNodeIndex(service, "build")
		if buildIndex < 0 || service.values[buildIndex].kind != envNodeMapping {
			continue
		}
		build := service.values[buildIndex]
		contextIndex := envNodeIndex(build, "context")
		if contextIndex >= 0 && build.values[contextIndex].kind != envNodeNull {
			if value, ok := envNodeString(build.values[contextIndex]); !ok || strings.TrimSpace(value) != "" {
				// Non-string contexts are reported by the strict decoder.
				continue
			}
		}
		missing = append(missing, services.keys[i])
	}
	sort.Strings(missing)
	return missing
}

// decodeYAMLNodeStrict decodes node with unknown-field rejection. yaml.v3
// only enforces KnownFields on a Decoder, so the node is re-encoded first;
// line numbers in the error are mapped back to the original document.
func decodeYAMLNodeStrict(node *yaml.Node, out any) error {
	data, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return remapYAMLErrorLines(err, yamlLineMap(node, data))
	}
	return nil
}

var yamlErrorLinePattern = regexp.MustCompile(`line (\d+)`)

// remapYAMLErrorLines rewrites "line N" references from a re-encoded
// document to the matching original line. References that cannot be mapped
// are removed rather than pointing at an unrelated line.
func remapYAMLErrorLines(err error, lines map[int]int) error {
	if err == nil {
		return nil
	}
	clean := func(message string) string { return remapYAMLLineReferences(message, lines) }
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		remapped := make([]string, len(typeErr.Errors))
		for i, entry := range typeErr.Errors {
			remapped[i] = clean(entry)
		}
		return &yaml.TypeError{Errors: remapped}
	}
	return errors.New(clean(err.Error()))
}

// remapYAMLLineReferences rewrites every "line N" in message through lines,
// dropping references that cannot be mapped.
func remapYAMLLineReferences(message string, lines map[int]int) string {
	message = yamlErrorLinePattern.ReplaceAllStringFunc(message, func(match string) string {
		number, convErr := strconv.Atoi(strings.TrimPrefix(match, "line "))
		if convErr == nil {
			if original, ok := lines[number]; ok {
				return fmt.Sprintf("line %d", original)
			}
		}
		return "line ?"
	})
	message = strings.ReplaceAll(message, "line ?: ", "")
	return strings.ReplaceAll(message, "line ?", "")
}

// yamlLineMap pairs the nodes of built with the nodes parsed back from its
// encoding and maps each encoded line to the original line of the node that
// produced it.
func yamlLineMap(built *yaml.Node, encoded []byte) map[int]int {
	var reparsed yaml.Node
	if err := yaml.Unmarshal(encoded, &reparsed); err != nil {
		return nil
	}
	root := &reparsed
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	lines := map[int]int{}
	var walk func(original, encodedNode *yaml.Node)
	walk = func(original, encodedNode *yaml.Node) {
		if original == nil || encodedNode == nil {
			return
		}
		// Children first: a mapping starts on the line of its first key, and
		// that key is the more precise source when the mapping was rebuilt
		// without some of its original keys.
		for i := 0; i < len(original.Content) && i < len(encodedNode.Content); i++ {
			walk(original.Content[i], encodedNode.Content[i])
		}
		if original.Line > 0 {
			if _, ok := lines[encodedNode.Line]; !ok {
				lines[encodedNode.Line] = original.Line
			}
		}
	}
	walk(built, root)
	return lines
}

func isYAMLMergeKey(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == "<<" && node.ShortTag() == "!!merge"
}

// flattenYAMLMergeKeys rewrites << merge keys into explicit keys with YAML
// precedence (explicit keys win, then earlier merge sources). Aliases must
// already be materialized.
func flattenYAMLMergeKeys(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode, yaml.DocumentNode:
		clone := *node
		clone.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			clone.Content[i] = flattenYAMLMergeKeys(child)
		}
		return &clone
	case yaml.MappingNode:
		explicit := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if !isYAMLMergeKey(node.Content[i]) {
				explicit[node.Content[i].Value] = true
			}
		}
		clone := *node
		clone.Content = nil
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if !isYAMLMergeKey(key) {
				clone.Content = append(clone.Content, key, flattenYAMLMergeKeys(value))
				continue
			}
			sources := []*yaml.Node{value}
			if value.Kind == yaml.SequenceNode {
				sources = value.Content
			}
			for _, source := range sources {
				flattened := flattenYAMLMergeKeys(source)
				if flattened == nil || flattened.Kind != yaml.MappingNode {
					continue
				}
				for j := 0; j+1 < len(flattened.Content); j += 2 {
					name := flattened.Content[j].Value
					if explicit[name] || seen[name] {
						continue
					}
					seen[name] = true
					clone.Content = append(clone.Content, flattened.Content[j], flattened.Content[j+1])
				}
			}
		}
		return &clone
	default:
		return node
	}
}

func yamlDeref(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode && depth < maxYAMLAliasExpansionDepth; depth++ {
		node = node.Alias
	}
	return node
}

// yamlHasKey reports whether a mapping defines key directly or through a <<
// merge, following aliases without materializing the document.
func yamlHasKey(node *yaml.Node, key string, depth int) bool {
	node = yamlDeref(node)
	if node == nil || node.Kind != yaml.MappingNode || depth > maxYAMLAliasExpansionDepth {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isYAMLMergeKey(node.Content[i]) {
			value := yamlDeref(node.Content[i+1])
			sources := []*yaml.Node{value}
			if value != nil && value.Kind == yaml.SequenceNode {
				sources = value.Content
			}
			for _, source := range sources {
				if yamlHasKey(source, key, depth+1) {
					return true
				}
			}
			continue
		}
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

// yamlEnvironmentValues returns every environment value node, including
// environments merged in with <<.
func yamlEnvironmentValues(node *yaml.Node, depth int) []*yaml.Node {
	node = yamlDeref(node)
	if node == nil || node.Kind != yaml.MappingNode || depth > maxYAMLAliasExpansionDepth {
		return nil
	}
	var values []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isYAMLMergeKey(node.Content[i]) {
			value := yamlDeref(node.Content[i+1])
			sources := []*yaml.Node{value}
			if value != nil && value.Kind == yaml.SequenceNode {
				sources = value.Content
			}
			for _, source := range sources {
				values = append(values, yamlEnvironmentValues(source, depth+1)...)
			}
			continue
		}
		values = append(values, node.Content[i+1])
	}
	return values
}

func yamlDocumentNeedsResolution(root *yaml.Node) bool {
	var environments *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if strings.HasPrefix(key.Value, extensionKeyPrefix) {
			return true
		}
		if key.Value == "environments" && environments == nil {
			environments = root.Content[i+1]
		}
	}
	for _, env := range yamlEnvironmentValues(environments, 0) {
		if yamlHasKey(env, environmentExtendsKey, 0) || yamlHasKey(env, environmentAbstractKey, 0) {
			return true
		}
	}
	return false
}

func yamlToEnvNode(node *yaml.Node) *envNode {
	switch node.Kind {
	case yaml.MappingNode:
		out := newEnvMapping()
		out.yaml = node
		for i := 0; i+1 < len(node.Content); i += 2 {
			out.appendEntry(node.Content[i].Value, node.Content[i], yamlToEnvNode(node.Content[i+1]))
		}
		return out
	case yaml.SequenceNode:
		return &envNode{kind: envNodeSequence, yaml: node}
	case yaml.ScalarNode:
		if node.ShortTag() == "!!null" {
			return &envNode{kind: envNodeNull, yaml: node}
		}
		return &envNode{kind: envNodeScalar, yaml: node}
	default:
		return &envNode{kind: envNodeScalar, yaml: node}
	}
}

func envNodeToYAML(node *envNode) *yaml.Node {
	if node.kind != envNodeMapping {
		return node.yaml
	}
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if node.yaml != nil {
		out.Line = node.yaml.Line
		out.Column = node.yaml.Column
		out.Style = node.yaml.Style
	}
	for i, key := range node.keys {
		keyNode := node.keyNodes[i]
		if keyNode == nil {
			keyNode = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
		}
		out.Content = append(out.Content, keyNode, envNodeToYAML(node.values[i]))
	}
	return out
}

// ---- JSON adapter ----

type jsonObjectEntry struct {
	key   string
	value json.RawMessage
}

// parseJSONObject decodes one JSON object into ordered raw entries. A
// duplicate key replaces the earlier value, matching encoding/json.
func parseJSONObject(data []byte) ([]jsonObjectEntry, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	var entries []jsonObjectEntry
	index := map[string]int{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		if i, ok := index[key]; ok {
			entries[i].value = value
			continue
		}
		index[key] = len(entries)
		entries = append(entries, jsonObjectEntry{key: key, value: value})
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false // trailing data; the strict decoder reports it
	}
	return entries, true
}

func jsonValueKind(raw json.RawMessage) byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

func jsonToEnvNode(raw json.RawMessage) *envNode {
	switch jsonValueKind(raw) {
	case '{':
		entries, ok := parseJSONObject(raw)
		if !ok {
			return &envNode{kind: envNodeScalar, raw: raw}
		}
		out := newEnvMapping()
		for _, entry := range entries {
			out.appendEntry(entry.key, nil, jsonToEnvNode(entry.value))
		}
		return out
	case '[':
		return &envNode{kind: envNodeSequence, raw: raw}
	case 'n':
		return &envNode{kind: envNodeNull, raw: raw}
	default:
		return &envNode{kind: envNodeScalar, raw: raw}
	}
}

func writeEnvNodeJSON(buf *bytes.Buffer, node *envNode) {
	if node.kind != envNodeMapping {
		buf.Write(bytes.TrimSpace(node.raw))
		return
	}
	writeJSONObject(buf, len(node.keys), func(i int) (string, func()) {
		return node.keys[i], func() { writeEnvNodeJSON(buf, node.values[i]) }
	})
}

func writeJSONObject(buf *bytes.Buffer, count int, entry func(i int) (string, func())) {
	buf.WriteByte('{')
	for i := 0; i < count; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, writeValue := entry(i)
		encodedKey, _ := json.Marshal(key)
		buf.Write(encodedKey)
		buf.WriteByte(':')
		writeValue()
	}
	buf.WriteByte('}')
}

func resolveJSONConfigDocument(data []byte) ([]byte, *environmentResolution, error) {
	root, ok := parseJSONObject(data)
	if !ok {
		return data, nil, nil
	}
	needs := false
	environmentsIndex := -1
	for i, entry := range root {
		if strings.HasPrefix(entry.key, extensionKeyPrefix) {
			needs = true
		}
		if entry.key == "environments" {
			environmentsIndex = i
		}
	}
	var environments []jsonObjectEntry
	if environmentsIndex >= 0 && jsonValueKind(root[environmentsIndex].value) == '{' {
		environments, _ = parseJSONObject(root[environmentsIndex].value)
		for _, env := range environments {
			if jsonValueKind(env.value) != '{' {
				continue
			}
			fields, _ := parseJSONObject(env.value)
			for _, field := range fields {
				if field.key == environmentExtendsKey || field.key == environmentAbstractKey {
					needs = true
				}
			}
		}
	}
	if !needs {
		return data, nil, nil
	}

	var meta *environmentResolution
	var environmentsJSON []byte
	if environments != nil {
		declared := make([]declaredEnvironment, 0, len(environments))
		for _, entry := range environments {
			env, err := declareJSONEnvironment(entry.key, entry.value)
			if err != nil {
				return nil, nil, err
			}
			declared = append(declared, env)
		}
		resolved, resolution, err := resolveEnvironmentInheritance(declared)
		if err != nil {
			return nil, nil, err
		}
		meta = resolution
		var concrete []declaredEnvironment
		for _, env := range declared {
			if !env.abstract {
				concrete = append(concrete, env)
			}
		}
		var buf bytes.Buffer
		writeJSONObject(&buf, len(concrete), func(i int) (string, func()) {
			return concrete[i].name, func() { writeEnvNodeJSON(&buf, resolved[concrete[i].name]) }
		})
		environmentsJSON = buf.Bytes()
	}
	if meta == nil {
		meta = &environmentResolution{abstract: map[string]bool{}, parents: map[string]string{}, chains: map[string][]string{}, provenance: map[string]map[string]string{}}
	}

	var kept []jsonObjectEntry
	for i, entry := range root {
		if strings.HasPrefix(entry.key, extensionKeyPrefix) {
			continue
		}
		if i == environmentsIndex && environmentsJSON != nil {
			entry.value = environmentsJSON
		}
		kept = append(kept, entry)
	}
	var buf bytes.Buffer
	writeJSONObject(&buf, len(kept), func(i int) (string, func()) {
		return kept[i].key, func() { buf.Write(bytes.TrimSpace(kept[i].value)) }
	})
	buf.WriteByte('\n')
	return buf.Bytes(), meta, nil
}

func declareJSONEnvironment(name string, raw json.RawMessage) (declaredEnvironment, error) {
	env := declaredEnvironment{name: name, body: newEnvMapping()}
	body := raw
	if jsonValueKind(raw) == '{' {
		fields, ok := parseJSONObject(raw)
		if ok {
			var kept []jsonObjectEntry
			for _, field := range fields {
				switch field.key {
				case environmentExtendsKey:
					var target string
					if jsonValueKind(field.value) != '"' || json.Unmarshal(field.value, &target) != nil || strings.TrimSpace(target) == "" {
						return env, fmt.Errorf("environment %s: extends must be the name of another environment", name)
					}
					env.extends = target
					env.hasExtends = true
					continue
				case environmentAbstractKey:
					var abstract bool
					if kind := jsonValueKind(field.value); (kind != 't' && kind != 'f') || json.Unmarshal(field.value, &abstract) != nil {
						return env, fmt.Errorf("environment %s: abstract must be true or false", name)
					}
					env.abstract = abstract
					continue
				}
				kept = append(kept, field)
			}
			var buf bytes.Buffer
			writeJSONObject(&buf, len(kept), func(i int) (string, func()) {
				return kept[i].key, func() { buf.Write(bytes.TrimSpace(kept[i].value)) }
			})
			body = buf.Bytes()
		}
	}

	if jsonValueKind(body) == '{' {
		env.body = jsonToEnvNode(body)
	}
	decodeBody := body
	if env.relaxesBuildContext() && jsonValueKind(body) == '{' {
		var buf bytes.Buffer
		writeEnvNodeJSON(&buf, withPlaceholderBuildContexts(env.body, true))
		decodeBody = buf.Bytes()
	}
	var decoded EnvironmentConfig
	decoder := json.NewDecoder(bytes.NewReader(decodeBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return env, &configDecodeError{err: fmt.Errorf("environment %s: %w", name, err)}
	}
	return env, nil
}

// ---- Config accessors ----

// EnvironmentExtendsChain returns the ancestors of env, nearest first, or nil
// when env does not extend another environment.
func (c *Config) EnvironmentExtendsChain(env string) []string {
	if c == nil || c.resolution == nil {
		return nil
	}
	chain := c.resolution.chains[env]
	if len(chain) == 0 {
		return nil
	}
	return append([]string{}, chain...)
}

// EnvironmentValueSource returns the declared environment that supplied the
// value at the dotted path (for example "services.web.replicas") of the
// resolved environment env. For a mapping path it returns the nearest
// environment in the chain that supplied any value below it. It returns ""
// when the path is not set or the config did not use inheritance.
func (c *Config) EnvironmentValueSource(env, path string) string {
	if c == nil || c.resolution == nil {
		return ""
	}
	prov := c.resolution.provenance[env]
	if owner, ok := prov[path]; ok {
		return owner
	}
	order := append([]string{env}, c.resolution.chains[env]...)
	rank := make(map[string]int, len(order))
	for i, name := range order {
		rank[name] = i
	}
	best := ""
	prefix := path + "."
	for key, owner := range prov {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if best == "" || rank[owner] < rank[best] {
			best = owner
		}
	}
	return best
}

// IsAbstractEnvironment reports whether name was declared with abstract: true.
func (c *Config) IsAbstractEnvironment(name string) bool {
	return c != nil && c.resolution != nil && c.resolution.abstract[name]
}

// environmentDescendants returns the sorted non-abstract environments that
// extend name directly or through other environments.
func (r *environmentResolution) concreteDescendants(name string) []string {
	var out []string
	for env, chain := range r.chains {
		if r.abstract[env] {
			continue
		}
		for _, ancestor := range chain {
			if ancestor == name {
				out = append(out, env)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// unusedAbstractEnvironments returns sorted abstract environments that no
// environment extends.
func (r *environmentResolution) unusedAbstractEnvironments() []string {
	extended := map[string]bool{}
	for _, parent := range r.parents {
		extended[parent] = true
	}
	var out []string
	for name := range r.abstract {
		if !extended[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

var environmentErrorPrefixPattern = regexp.MustCompile(`^environments?[ .]([a-z][a-z0-9_-]{0,62})(?:[ .:]|$)`)

// annotateEnvironmentChain appends the extends chain to a validation error
// that names an inheriting environment.
func (c *Config) annotateEnvironmentChain(err error) error {
	if err == nil || c == nil || c.resolution == nil {
		return err
	}
	match := environmentErrorPrefixPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return err
	}
	chain := c.resolution.chains[match[1]]
	if len(chain) == 0 {
		return err
	}
	return fmt.Errorf("%w (environment %s extends %s)", err, match[1], strings.Join(chain, " -> "))
}
