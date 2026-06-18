package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func formatDeployConfigError(configPath string, err error) error {
	message := strings.TrimSpace(err.Error())
	switch {
	case strings.HasPrefix(message, "failed to parse YAML config:"):
		detail := strings.TrimSpace(strings.TrimPrefix(message, "failed to parse YAML config:"))
		title := "YAML syntax error"
		if strings.Contains(detail, "unmarshal errors:") {
			title = "YAML config error"
		}
		return formatConfigParseError(title, configPath, detail, yamlErrorLine(detail), 0)
	case strings.HasPrefix(message, "failed to parse JSON config:"):
		detail := strings.TrimSpace(strings.TrimPrefix(message, "failed to parse JSON config:"))
		title := "JSON config error"
		line, column := jsonErrorLocation(configPath, err)
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			title = "JSON syntax error"
			if line > 0 && column > 0 {
				detail = fmt.Sprintf("line %d, column %d: %s", line, column, detail)
			}
		}
		return formatConfigParseError(title, configPath, detail, line, column)
	case strings.HasPrefix(message, "invalid config:"):
		return fmt.Errorf("config validation failed in %s:\n  %s", filepath.Clean(configPath), strings.TrimSpace(strings.TrimPrefix(message, "invalid config:")))
	default:
		return fmt.Errorf("config preflight failed in %s:\n  %s", filepath.Clean(configPath), message)
	}
}

func formatConfigParseError(title string, configPath string, detail string, line int, column int) error {
	var out strings.Builder
	fmt.Fprintf(&out, "%s in %s:\n  %s", title, filepath.Clean(configPath), detail)
	if context := configSourceContext(configPath, line, column); context != "" {
		out.WriteString("\n")
		out.WriteString(context)
		out.WriteString("\n  Check indentation, brackets, quotes, and separators near the highlighted line.")
	}
	return errors.New(out.String())
}

func yamlErrorLine(detail string) int {
	if line := parseLineAfterMarker(detail, "yaml: line "); line > 0 {
		return line
	}
	return parseLineAfterMarker(detail, "line ")
}

func parseLineAfterMarker(value string, marker string) int {
	index := strings.Index(value, marker)
	if index < 0 {
		return 0
	}
	start := index + len(marker)
	end := start
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	line, err := strconv.Atoi(value[start:end])
	if err != nil {
		return 0
	}
	return line
}

func jsonErrorLocation(configPath string, err error) (int, int) {
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) || syntaxErr.Offset <= 0 {
		return 0, 0
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		return 0, 0
	}
	return lineColumnForOffset(data, syntaxErr.Offset)
}

func lineColumnForOffset(data []byte, offset int64) (int, int) {
	line := 1
	column := 1
	if offset < 1 {
		return line, column
	}
	for index, r := range string(data) {
		if int64(index) >= offset-1 {
			break
		}
		if r == '\n' {
			line++
			column = 1
			continue
		}
		column++
	}
	return line, column
}

func configSourceContext(configPath string, line int, column int) string {
	if line <= 0 {
		return ""
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if line > len(lines) {
		return ""
	}
	source := lines[line-1]
	var out strings.Builder
	fmt.Fprintf(&out, "  %d | %s", line, source)
	if column > 0 {
		out.WriteString("\n")
		out.WriteString("    | ")
		if column > 1 {
			out.WriteString(strings.Repeat(" ", column-1))
		}
		out.WriteString("^")
	}
	return out.String()
}
