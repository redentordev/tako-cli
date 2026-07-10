package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

type stringOrListForm uint8

const (
	stringOrListUnset stringOrListForm = iota
	stringOrListScalar
	stringOrListSequence
)

// StringOrList preserves whether a config value used scalar (shell/shorthand)
// or list (exec/argv) form while supporting both YAML and JSON.
type StringOrList struct {
	form   stringOrListForm
	scalar string
	list   []string
}

// StringValue constructs scalar form.
func StringValue(value string) StringOrList {
	return StringOrList{form: stringOrListScalar, scalar: value}
}

// ListValue constructs list/argv form.
func ListValue(values ...string) StringOrList {
	return StringOrList{form: stringOrListSequence, list: append([]string(nil), values...)}
}

// IsSet reports whether the field was supplied, including an explicitly empty
// scalar or list (which validation can reject).
func (v StringOrList) IsSet() bool { return v.form != stringOrListUnset }

// IsList reports whether list form was supplied.
func (v StringOrList) IsList() bool { return v.form == stringOrListSequence }

// IsZero supports omitempty for unset values.
func (v StringOrList) IsZero() bool { return !v.IsSet() }

// Scalar returns scalar form and whether the value used that form.
func (v StringOrList) Scalar() (string, bool) {
	return v.scalar, v.form == stringOrListScalar
}

// Arguments returns a defensive copy of list form, or a one-element list for
// scalar form. Unset values return nil.
func (v StringOrList) Arguments() []string {
	switch v.form {
	case stringOrListScalar:
		return []string{v.scalar}
	case stringOrListSequence:
		return append([]string(nil), v.list...)
	default:
		return nil
	}
}

// ContainerCommand converts legacy scalar command form to its existing
// shell behavior and passes list form through as raw argv.
func (v StringOrList) ContainerCommand() []string {
	if v.form == stringOrListScalar {
		return []string{"sh", "-c", v.scalar}
	}
	return v.Arguments()
}

func (v *StringOrList) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*v = StringOrList{}
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("must be a string or list of strings")
		}
		*v = StringValue(node.Value)
		return nil
	case yaml.SequenceNode:
		values := make([]string, len(node.Content))
		for i, child := range node.Content {
			if child.Kind != yaml.ScalarNode || child.Tag != "!!str" {
				return fmt.Errorf("list item %d must be a string", i)
			}
			values[i] = child.Value
		}
		*v = ListValue(values...)
		return nil
	default:
		return fmt.Errorf("must be a string or list of strings")
	}
}

func (v StringOrList) MarshalYAML() (any, error) {
	switch v.form {
	case stringOrListScalar:
		return v.scalar, nil
	case stringOrListSequence:
		return append([]string(nil), v.list...), nil
	default:
		return nil, nil
	}
}

func (v *StringOrList) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*v = StringOrList{}
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var scalar string
		if err := json.Unmarshal(data, &scalar); err != nil {
			return err
		}
		*v = StringValue(scalar)
		return nil
	}
	if len(data) > 0 && data[0] == '[' {
		var values []string
		if err := json.Unmarshal(data, &values); err != nil {
			return fmt.Errorf("must be a string or list of strings: %w", err)
		}
		*v = ListValue(values...)
		return nil
	}
	return fmt.Errorf("must be a string or list of strings")
}

func (v StringOrList) MarshalJSON() ([]byte, error) {
	switch v.form {
	case stringOrListScalar:
		return json.Marshal(v.scalar)
	case stringOrListSequence:
		return json.Marshal(v.list)
	default:
		return []byte("null"), nil
	}
}
