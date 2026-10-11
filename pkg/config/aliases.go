package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Alias value forms reported by AliasForm and machine output.
const (
	AliasFormList   = "list"
	AliasFormString = "string"
)

// aliasNamePattern is the allowed alias name format: lowercase letters,
// digits, hyphen and underscore, starting with a letter or digit, at most 63
// characters.
var aliasNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// reservedAliasNames can never be alias names: cobra's help/completion
// machinery and the `tako aliases` command itself.
var reservedAliasNames = map[string]bool{
	"help":             true,
	"completion":       true,
	"__complete":       true,
	"__completeNoDesc": true,
	"aliases":          true,
}

// forbiddenAliasFlags may not appear in alias values before a `--`
// separator: a committed alias must not redirect the config source or
// weaken SSH host key verification.
var forbiddenAliasFlags = []string{"--config", "--host-key-mode"}

// AliasMap is the top-level `aliases` map: alias name -> Tako arguments in
// list form (used verbatim) or string form (split by SplitAliasString).
// Decoding errors name the offending `aliases.<name>` entry.
type AliasMap map[string]StringOrList

// UnmarshalYAML decodes the aliases mapping, attributing value errors to the
// alias that caused them.
func (m *AliasMap) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*m = nil
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("aliases: must be a map of alias name to command arguments (line %d)", node.Line)
	}
	result := make(AliasMap, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("aliases: alias names must be strings (line %d)", key.Line)
		}
		name := key.Value
		if _, exists := result[name]; exists {
			return fmt.Errorf("aliases.%s: alias defined more than once (line %d)", name, key.Line)
		}
		var parsed StringOrList
		if err := parsed.UnmarshalYAML(value); err != nil {
			return fmt.Errorf("aliases.%s: %v (line %d)", name, err, value.Line)
		}
		result[name] = parsed
	}
	*m = result
	return nil
}

// UnmarshalJSON decodes the aliases object, attributing value errors to the
// alias that caused them.
func (m *AliasMap) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("aliases: must be an object of alias name to command arguments: %w", err)
	}
	if raw == nil {
		*m = nil
		return nil
	}
	result := make(AliasMap, len(raw))
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		var parsed StringOrList
		if err := parsed.UnmarshalJSON(raw[name]); err != nil {
			return fmt.Errorf("aliases.%s: %v", name, err)
		}
		if !parsed.IsSet() {
			return fmt.Errorf("aliases.%s: must be a string or list of strings", name)
		}
		result[name] = parsed
	}
	*m = result
	return nil
}

// Names returns the alias names in sorted order.
func (m AliasMap) Names() []string {
	return slices.Sorted(maps.Keys(m))
}

// AliasForm reports whether an alias value used list or string form.
func AliasForm(value StringOrList) string {
	if value.IsList() {
		return AliasFormList
	}
	return AliasFormString
}

// AliasArguments returns the Tako arguments an alias value expands to: list
// form verbatim, string form split by SplitAliasString.
func AliasArguments(value StringOrList) ([]string, error) {
	if scalar, ok := value.Scalar(); ok {
		return SplitAliasString(scalar)
	}
	if !value.IsSet() {
		return nil, fmt.Errorf("must be a string or list of strings")
	}
	args := value.Arguments()
	if len(args) == 0 {
		return nil, fmt.Errorf("must contain at least one argument")
	}
	return args, nil
}

// SplitAliasString splits a string-form alias into arguments with POSIX-shell
// -like word splitting and nothing else: unquoted whitespace separates words,
// '...' is literal, "..." honors \" and \\ escapes, and a backslash outside
// quotes escapes the next character. There is no variable expansion,
// globbing, tilde expansion, command substitution, or operator handling, so
// `;`, `|`, `&&`, `$(x)` and `*` are ordinary argument text.
func SplitAliasString(value string) ([]string, error) {
	var words []string
	var current strings.Builder
	inWord := false

	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		switch {
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
			i += size
		case r == '\'':
			end := strings.IndexByte(value[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			current.WriteString(value[i+1 : i+1+end])
			inWord = true
			i += end + 2
		case r == '"':
			inWord = true
			i++
			closed := false
			for i < len(value) {
				c := value[i]
				if c == '"' {
					closed = true
					i++
					break
				}
				if c == '\\' && i+1 < len(value) && (value[i+1] == '"' || value[i+1] == '\\') {
					current.WriteByte(value[i+1])
					i += 2
					continue
				}
				current.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated double quote")
			}
		case r == '\\':
			if i+1 >= len(value) {
				return nil, fmt.Errorf("trailing backslash")
			}
			next, nextSize := utf8.DecodeRuneInString(value[i+1:])
			current.WriteRune(next)
			inWord = true
			i += 1 + nextSize
		default:
			current.WriteRune(r)
			inWord = true
			i += size
		}
	}
	if inWord {
		words = append(words, current.String())
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("must contain at least one argument")
	}
	return words, nil
}

// IsReservedAliasName reports whether name can never be used as an alias.
func IsReservedAliasName(name string) bool {
	return reservedAliasNames[name]
}

// ValidateAliases checks alias names and values without knowledge of the
// CLI command tree: name format, reserved names, value parsing, the leading
// command token, and forbidden global flags. Whether the target is a visible
// built-in command (and not another alias), and whether a built-in shadows
// the alias, is checked by the CLI.
func ValidateAliases(aliases AliasMap) error {
	for _, name := range aliases.Names() {
		label := "aliases." + name
		if reservedAliasNames[name] {
			return fmt.Errorf("%s: alias name %q is reserved", label, name)
		}
		if !aliasNamePattern.MatchString(name) {
			return fmt.Errorf("%s: invalid alias name %q: must match ^[a-z0-9][a-z0-9_-]*$ and be at most 63 characters", label, name)
		}
		args, err := AliasArguments(aliases[name])
		if err != nil {
			return fmt.Errorf("%s: %v", label, err)
		}
		first := args[0]
		switch {
		case strings.TrimSpace(first) == "":
			return fmt.Errorf("%s: first argument must name a Tako command", label)
		case strings.HasPrefix(first, "-"):
			return fmt.Errorf("%s: first argument must name a Tako command, not the flag %q", label, first)
		case first == "tako":
			return fmt.Errorf("%s: omit the leading \"tako\"; alias values are Tako arguments such as [exec, web, --, sh]", label)
		}
		// Alias-to-alias references are rejected by the CLI, which knows the
		// built-in commands: a first argument that names both a built-in and
		// a (shadowed) alias refers to the built-in.
		for _, arg := range args {
			if arg == "--" {
				break
			}
			for _, flag := range forbiddenAliasFlags {
				if arg == flag || strings.HasPrefix(arg, flag+"=") {
					return fmt.Errorf("%s: %s is not allowed in aliases; pass it on the command line instead", label, flag)
				}
			}
		}
		for _, arg := range args {
			if hasConfigControlChars(arg) {
				return fmt.Errorf("%s: arguments must not contain control characters", label)
			}
		}
	}
	return nil
}

type rawAliasesDocument struct {
	Aliases map[string]any `yaml:"aliases" json:"aliases"`
}

// validateRawAliases rejects ${...} references in alias values before
// environment expansion: aliases are literal, and a missing variable must
// not mask the real problem.
func validateRawAliases(data []byte, isJSON bool) error {
	var doc rawAliasesDocument
	if isJSON {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil // the strict parse after expansion reports the real error
		}
	} else {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Aliases)) {
		if rawAliasValueHasEnvRef(doc.Aliases[name]) {
			return fmt.Errorf("aliases.%s: aliases do not support ${...} references; alias values are used literally", name)
		}
	}
	return nil
}

func rawAliasValueHasEnvRef(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, "${")
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.Contains(s, "${") {
				return true
			}
		}
	}
	return false
}

// ConfigNotFoundError reports that no config file exists at the resolved
// path.
type ConfigNotFoundError struct {
	Path string
}

func (e *ConfigNotFoundError) Error() string {
	return fmt.Sprintf("config file not found: %s", e.Path)
}

// IsConfigNotFound reports whether err is a ConfigNotFoundError.
func IsConfigNotFound(err error) bool {
	var notFound *ConfigNotFoundError
	return errors.As(err, &notFound)
}

// ProjectAliases is the result of LoadAliases.
type ProjectAliases struct {
	// ConfigPath is the config file the aliases were read from.
	ConfigPath string
	Aliases    AliasMap
}

// ResolveConfigPath returns the config file Tako uses: the explicit path, or
// tako.yaml, then tako.json in the working directory (tako.yaml when
// neither exists).
func ResolveConfigPath(configPath string) string {
	if configPath != "" {
		return configPath
	}
	if _, err := os.Stat("tako.yaml"); err == nil {
		return "tako.yaml"
	}
	if _, err := os.Stat("tako.json"); err == nil {
		return "tako.json"
	}
	return "tako.yaml"
}

// LoadAliases reads only the top-level `aliases` section of a config file.
// Unlike LoadConfig it does not load .env, expand ${...} references, validate
// other sections, or read platform state, so alias lookup works even when
// unrelated sections reference unset variables. A missing file returns a
// *ConfigNotFoundError.
func LoadAliases(configPath string) (*ProjectAliases, error) {
	path := ResolveConfigPath(configPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ConfigNotFoundError{Path: path}
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	isJSON := strings.ToLower(filepath.Ext(path)) == ".json"
	if err := validateRawAliases(data, isJSON); err != nil {
		return nil, err
	}

	var aliases AliasMap
	if isJSON {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config: %w", err)
		}
		if raw, ok := doc["aliases"]; ok {
			if err := json.Unmarshal(raw, &aliases); err != nil {
				return nil, err
			}
		}
	} else {
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config: %w", err)
		}
		if section := topLevelYAMLValue(&doc, "aliases"); section != nil {
			if err := section.Decode(&aliases); err != nil {
				return nil, err
			}
		}
	}
	if err := ValidateAliases(aliases); err != nil {
		return nil, err
	}
	return &ProjectAliases{ConfigPath: path, Aliases: aliases}, nil
}

func topLevelYAMLValue(doc *yaml.Node, key string) *yaml.Node {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}
