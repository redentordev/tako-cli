package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// UlimitConfig supports Compose's scalar limit shorthand and soft/hard form.
type UlimitConfig struct {
	Soft int64 `yaml:"soft" json:"soft"`
	Hard int64 `yaml:"hard" json:"hard"`
}

func (u *UlimitConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if len(data) > 0 && data[0] != '{' {
		var value int64
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("ulimit must be an integer or soft/hard object")
		}
		u.Soft, u.Hard = value, value
		return nil
	}
	type plain UlimitConfig
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*plain)(u)); err != nil {
		return fmt.Errorf("invalid ulimit: %w", err)
	}
	return nil
}

func (u *UlimitConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return fmt.Errorf("ulimit is required")
	}
	if node.Kind == yaml.ScalarNode {
		var value int64
		if err := node.Decode(&value); err != nil {
			return fmt.Errorf("ulimit must be an integer or soft/hard mapping")
		}
		u.Soft, u.Hard = value, value
		return nil
	}
	type plain UlimitConfig
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("ulimit must be an integer or soft/hard mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "soft" && node.Content[i].Value != "hard" {
			return fmt.Errorf("unknown ulimit field %q", node.Content[i].Value)
		}
	}
	return node.Decode((*plain)(u))
}
