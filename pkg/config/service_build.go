package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type structuredServiceBuild struct {
	Context string            `yaml:"context" json:"context"`
	Args    map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
	Target  string            `yaml:"target,omitempty" json:"target,omitempty"`
}

// UnmarshalJSON provides the same scalar/object build union for strict JSON
// config files. The inner alias decoder preserves unknown-field rejection.
func (s *ServiceConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var buildArgs map[string]string
	var buildTarget string
	structured := false
	if raw, ok := fields["build"]; ok && len(raw) > 0 && raw[0] == '{' {
		var build structuredServiceBuild
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&build); err != nil {
			return fmt.Errorf("invalid structured build: %w", err)
		}
		if strings.TrimSpace(build.Context) == "" {
			return fmt.Errorf("invalid structured build: context is required")
		}
		contextData, _ := json.Marshal(build.Context)
		fields["build"] = contextData
		buildArgs = build.Args
		buildTarget = build.Target
		structured = true
	}
	plainData, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	type plainServiceConfig ServiceConfig
	decoder := json.NewDecoder(bytes.NewReader(plainData))
	decoder.DisallowUnknownFields()
	var decoded plainServiceConfig
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*s = ServiceConfig(decoded)
	s.BuildArgs = buildArgs
	s.BuildTarget = buildTarget
	s.buildStructured = structured
	return nil
}

// MarshalJSON mirrors MarshalYAML so API/config JSON preserves structured
// build options without exposing internal helper fields.
func (s ServiceConfig) MarshalJSON() ([]byte, error) {
	type plainServiceConfig ServiceConfig
	data, err := json.Marshal(plainServiceConfig(s))
	if err != nil {
		return nil, err
	}
	if !s.buildStructured && len(s.BuildArgs) == 0 && s.BuildTarget == "" {
		return data, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	buildData, err := json.Marshal(structuredServiceBuild{Context: s.Build, Args: s.BuildArgs, Target: s.BuildTarget})
	if err != nil {
		return nil, err
	}
	fields["build"] = buildData
	return json.Marshal(fields)
}

// UnmarshalYAML accepts the legacy scalar build context and the structured
// context/args/target form while keeping ServiceConfig.Build source-compatible.
func (s *ServiceConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return fmt.Errorf("service must be a mapping")
	}
	resolved, err := materializeYAMLAliases(node)
	if err != nil {
		return err
	}
	copyNode := *resolved
	structured := false
	var buildArgs map[string]string
	var buildTarget string
	if value, valueIndex := yamlMappingValue(&copyNode, "build"); value != nil {
		if value.Kind == yaml.MappingNode {
			var build structuredServiceBuild
			data, err := yaml.Marshal(value)
			if err != nil {
				return err
			}
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			if err := decoder.Decode(&build); err != nil {
				return fmt.Errorf("invalid structured build: %w", err)
			}
			if strings.TrimSpace(build.Context) == "" {
				return fmt.Errorf("invalid structured build: context is required")
			}
			contextNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: build.Context}
			if valueIndex >= 0 {
				copyNode.Content[valueIndex] = contextNode
			} else {
				copyNode.Content = append(copyNode.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "build"},
					contextNode,
				)
			}
			buildArgs = build.Args
			buildTarget = build.Target
			structured = true
		}
	}

	type plainServiceConfig ServiceConfig
	data, err := yaml.Marshal(&copyNode)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var decoded plainServiceConfig
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*s = ServiceConfig(decoded)
	s.BuildArgs = buildArgs
	s.BuildTarget = buildTarget
	s.buildStructured = structured
	return nil
}

// yamlMappingValue returns a mapping value using YAML merge precedence. The
// index is the value's position when it is explicit in node, or -1 when it was
// inherited through <<. Aliases have already been materialized.
func yamlMappingValue(node *yaml.Node, key string) (*yaml.Node, int) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key && node.Content[i].Value != "<<" {
			return node.Content[i+1], i + 1
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "<<" {
			continue
		}
		merged := node.Content[i+1]
		if merged.Kind == yaml.MappingNode {
			value, _ := yamlMappingValue(merged, key)
			if value != nil {
				return value, -1
			}
		}
		if merged.Kind == yaml.SequenceNode {
			for _, candidate := range merged.Content {
				value, _ := yamlMappingValue(candidate, key)
				if value != nil {
					return value, -1
				}
			}
		}
	}
	return nil, -1
}

// materializeYAMLAliases clones a YAML node and replaces aliases with their
// anchored values. ServiceConfig has a custom decoder for the build union and
// therefore re-serializes one service node at a time; unresolved aliases that
// point to anchors elsewhere in the document would otherwise be orphaned.
// Merge keys remain merge keys, so yaml.v3 applies normal YAML precedence when
// the isolated service is strictly decoded below.
const (
	maxYAMLAliasExpansionNodes = 100000
	maxYAMLAliasExpansionDepth = 100
)

type yamlAliasMaterializer struct {
	visiting map[*yaml.Node]bool
	nodes    int
}

func materializeYAMLAliases(node *yaml.Node) (*yaml.Node, error) {
	materializer := yamlAliasMaterializer{visiting: make(map[*yaml.Node]bool)}
	return materializer.materialize(node, 0)
}

func (m *yamlAliasMaterializer) materialize(node *yaml.Node, depth int) (*yaml.Node, error) {
	if node == nil {
		return nil, nil
	}
	if depth > maxYAMLAliasExpansionDepth {
		return nil, fmt.Errorf("YAML alias expansion exceeds maximum depth %d", maxYAMLAliasExpansionDepth)
	}
	m.nodes++
	if m.nodes > maxYAMLAliasExpansionNodes {
		return nil, fmt.Errorf("YAML alias expansion exceeds maximum size %d nodes", maxYAMLAliasExpansionNodes)
	}
	if node.Kind == yaml.AliasNode {
		if node.Alias == nil {
			return nil, fmt.Errorf("YAML alias %q has no target", node.Value)
		}
		if m.visiting[node.Alias] {
			return nil, fmt.Errorf("cyclic YAML alias %q", node.Value)
		}
		m.visiting[node.Alias] = true
		resolved, err := m.materialize(node.Alias, depth+1)
		delete(m.visiting, node.Alias)
		return resolved, err
	}

	clone := *node
	clone.Anchor = ""
	clone.Alias = nil
	clone.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		resolved, err := m.materialize(child, depth+1)
		if err != nil {
			return nil, err
		}
		clone.Content[i] = resolved
	}
	return &clone, nil
}

// MarshalYAML preserves structured build form when it was configured or when
// build options are present. Simple build contexts remain scalar.
func (s ServiceConfig) MarshalYAML() (any, error) {
	type plainServiceConfig ServiceConfig
	var document yaml.Node
	if err := document.Encode(plainServiceConfig(s)); err != nil {
		return nil, err
	}
	if !s.buildStructured && len(s.BuildArgs) == 0 && s.BuildTarget == "" {
		return &document, nil
	}
	root := &document
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "build" {
			continue
		}
		var value yaml.Node
		if err := value.Encode(structuredServiceBuild{Context: s.Build, Args: s.BuildArgs, Target: s.BuildTarget}); err != nil {
			return nil, err
		}
		root.Content[i+1] = &value
		break
	}
	return &document, nil
}
