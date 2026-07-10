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
	copyNode := *node
	copyNode.Content = append([]*yaml.Node(nil), node.Content...)
	structured := false
	var buildArgs map[string]string
	var buildTarget string
	for i := 0; i+1 < len(copyNode.Content); i += 2 {
		if copyNode.Content[i].Value != "build" {
			continue
		}
		value := copyNode.Content[i+1]
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
			copyNode.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: build.Context}
			buildArgs = build.Args
			buildTarget = build.Target
			structured = true
		}
		break
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
