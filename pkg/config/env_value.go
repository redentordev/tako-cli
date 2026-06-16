package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultSharedPortName = "default"

// EnvValue is a service environment value. Most values are plain strings, but
// link/url forms let Tako resolve service references without exposing import
// mechanics in normal app config.
type EnvValue struct {
	Value string          `yaml:"-" json:"-"`
	Link  *ServiceLinkRef `yaml:"link,omitempty" json:"link,omitempty"`
	URL   string          `yaml:"url,omitempty" json:"url,omitempty"`
}

type ServiceLinkRef struct {
	App     string   `yaml:"app,omitempty" json:"app,omitempty"`
	Stage   string   `yaml:"stage,omitempty" json:"stage,omitempty"`
	Service string   `yaml:"service,omitempty" json:"service,omitempty"`
	Port    string   `yaml:"port,omitempty" json:"port,omitempty"`
	Servers []string `yaml:"servers,omitempty" json:"servers,omitempty"`
}

type ServiceShareConfig struct {
	Enabled bool     `yaml:"-" json:"-"`
	Ports   []string `yaml:"ports,omitempty" json:"ports,omitempty"`
}

func PlainEnvValue(value string) EnvValue {
	return EnvValue{Value: value}
}

func (v EnvValue) IsPlain() bool {
	return v.Link == nil && strings.TrimSpace(v.URL) == ""
}

func (v EnvValue) PlainString() string {
	if v.IsPlain() {
		return v.Value
	}
	if strings.TrimSpace(v.URL) != "" {
		return strings.TrimSpace(v.URL)
	}
	return ""
}

func (v *EnvValue) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var value string
		if err := node.Decode(&value); err != nil {
			return err
		}
		*v = EnvValue{Value: value}
		return nil
	case yaml.MappingNode:
		if err := rejectUnknownYAMLFields(node, map[string]bool{"link": true, "url": true}); err != nil {
			return err
		}
		var raw struct {
			Link *ServiceLinkRef `yaml:"link"`
			URL  string          `yaml:"url"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		if raw.Link == nil && strings.TrimSpace(raw.URL) == "" {
			return fmt.Errorf("env object must contain link or url")
		}
		if raw.Link != nil && strings.TrimSpace(raw.URL) != "" {
			return fmt.Errorf("env object cannot contain both link and url")
		}
		*v = EnvValue{Link: raw.Link, URL: strings.TrimSpace(raw.URL)}
		return nil
	default:
		return fmt.Errorf("env value must be a string or object")
	}
}

func (v EnvValue) MarshalYAML() (any, error) {
	if v.IsPlain() {
		return v.Value, nil
	}
	type raw EnvValue
	return raw(v), nil
}

func (v *EnvValue) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*v = EnvValue{Value: value}
		return nil
	}
	var raw struct {
		Link *ServiceLinkRef `json:"link"`
		URL  string          `json:"url"`
	}
	if err := rejectUnknownJSONFields(data, map[string]bool{"link": true, "url": true}); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Link == nil && strings.TrimSpace(raw.URL) == "" {
		return fmt.Errorf("env object must contain link or url")
	}
	if raw.Link != nil && strings.TrimSpace(raw.URL) != "" {
		return fmt.Errorf("env object cannot contain both link and url")
	}
	*v = EnvValue{Link: raw.Link, URL: strings.TrimSpace(raw.URL)}
	return nil
}

func (v EnvValue) MarshalJSON() ([]byte, error) {
	if v.IsPlain() {
		return json.Marshal(v.Value)
	}
	type raw EnvValue
	return json.Marshal(raw(v))
}

func (r *ServiceLinkRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var service string
		if err := node.Decode(&service); err != nil {
			return err
		}
		*r = ServiceLinkRef{Service: strings.TrimSpace(service)}
		return nil
	case yaml.MappingNode:
		if err := rejectUnknownYAMLFields(node, serviceLinkFields()); err != nil {
			return err
		}
		var raw struct {
			App         string   `yaml:"app"`
			Project     string   `yaml:"project"`
			Stage       string   `yaml:"stage"`
			Environment string   `yaml:"environment"`
			Service     string   `yaml:"service"`
			Port        string   `yaml:"port"`
			Servers     []string `yaml:"servers"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		*r = ServiceLinkRef{
			App:     firstNonEmpty(raw.App, raw.Project),
			Stage:   firstNonEmpty(raw.Stage, raw.Environment),
			Service: strings.TrimSpace(raw.Service),
			Port:    strings.TrimSpace(raw.Port),
			Servers: normalizeStringList(raw.Servers),
		}
		return nil
	default:
		return fmt.Errorf("link must be a service name or object")
	}
}

func (r *ServiceLinkRef) UnmarshalJSON(data []byte) error {
	var service string
	if err := json.Unmarshal(data, &service); err == nil {
		*r = ServiceLinkRef{Service: strings.TrimSpace(service)}
		return nil
	}
	var raw struct {
		App         string   `json:"app"`
		Project     string   `json:"project"`
		Stage       string   `json:"stage"`
		Environment string   `json:"environment"`
		Service     string   `json:"service"`
		Port        string   `json:"port"`
		Servers     []string `json:"servers"`
	}
	if err := rejectUnknownJSONFields(data, serviceLinkFields()); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = ServiceLinkRef{
		App:     firstNonEmpty(raw.App, raw.Project),
		Stage:   firstNonEmpty(raw.Stage, raw.Environment),
		Service: strings.TrimSpace(raw.Service),
		Port:    strings.TrimSpace(raw.Port),
		Servers: normalizeStringList(raw.Servers),
	}
	return nil
}

func (s *ServiceShareConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return err
		}
		*s = ServiceShareConfig{Enabled: enabled}
		return nil
	case yaml.SequenceNode:
		var ports []string
		if err := node.Decode(&ports); err != nil {
			return err
		}
		*s = ServiceShareConfig{Enabled: true, Ports: normalizeStringList(ports)}
		return nil
	case yaml.MappingNode:
		if err := rejectUnknownYAMLFields(node, map[string]bool{"ports": true}); err != nil {
			return err
		}
		var raw struct {
			Ports []string `yaml:"ports"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		*s = ServiceShareConfig{Enabled: true, Ports: normalizeStringList(raw.Ports)}
		return nil
	default:
		return fmt.Errorf("share must be true, false, a port list, or object")
	}
}

func (s *ServiceShareConfig) UnmarshalJSON(data []byte) error {
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		*s = ServiceShareConfig{Enabled: enabled}
		return nil
	}
	var ports []string
	if err := json.Unmarshal(data, &ports); err == nil {
		*s = ServiceShareConfig{Enabled: true, Ports: normalizeStringList(ports)}
		return nil
	}
	var raw struct {
		Ports []string `json:"ports"`
	}
	if err := rejectUnknownJSONFields(data, map[string]bool{"ports": true}); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = ServiceShareConfig{Enabled: true, Ports: normalizeStringList(raw.Ports)}
	return nil
}

func serviceLinkFields() map[string]bool {
	return map[string]bool{
		"app":         true,
		"project":     true,
		"stage":       true,
		"environment": true,
		"service":     true,
		"port":        true,
		"servers":     true,
	}
}

func rejectUnknownYAMLFields(node *yaml.Node, allowed map[string]bool) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("object keys must be strings")
		}
		key := keyNode.Value
		if !allowed[key] {
			return fmt.Errorf("unknown field %q", key)
		}
		if seen[key] {
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = true
	}
	return nil
}

func rejectUnknownJSONFields(data []byte, allowed map[string]bool) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func ResolveServicePort(serviceName string, service ServiceConfig, requested string) (PortConfig, error) {
	ports := service.EffectivePorts()
	if len(ports) == 0 {
		return PortConfig{}, fmt.Errorf("linked service %s has no service port", serviceName)
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(ports) != 1 {
			return PortConfig{}, fmt.Errorf("linked service %s has multiple ports; set link.port", serviceName)
		}
		return ports[0], nil
	}
	for _, port := range ports {
		if port.Name == requested {
			return port, nil
		}
	}
	return PortConfig{}, fmt.Errorf("linked service %s has no port %q", serviceName, requested)
}
