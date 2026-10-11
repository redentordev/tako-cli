package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/redentordev/tako-cli/pkg/secrets"
)

// maxSecretInputBytes bounds values read from stdin or files so a mistaken
// pipe (for example a binary or a huge log) fails instead of being stored.
const maxSecretInputBytes = 1 << 20

// resolveSecretSetArg turns the `tako secrets set` argument into a key and a
// value. Supported forms:
//
//	KEY=value               literal value (visible in argv; kept for compatibility)
//	KEY --stdin             value read from stdin
//	KEY --from-file PATH    value read from PATH
//
// A literal value is stored verbatim, including a leading '@'. Values read
// from stdin or a file have exactly one trailing newline removed. Errors never
// include the value.
func resolveSecretSetArg(arg string, useStdin bool, fromFile string, stdin io.Reader) (string, string, error) {
	key, value, hasValue := strings.Cut(arg, "=")
	if !useStdin && fromFile == "" {
		if !hasValue {
			return "", "", fmt.Errorf("invalid format, use: KEY=value, KEY --stdin, or KEY --from-file PATH")
		}
		return key, value, nil
	}
	if useStdin && fromFile != "" {
		return "", "", fmt.Errorf("--stdin and --from-file cannot be used together")
	}
	if hasValue {
		return "", "", fmt.Errorf("--stdin and --from-file take only the key: tako secrets set KEY --stdin | --from-file PATH")
	}
	if !secrets.IsValidSecretKey(key) {
		return "", "", fmt.Errorf("invalid secret key %q", key)
	}
	if useStdin {
		data, err := readSecretInput(stdin, "stdin")
		if err != nil {
			return "", "", err
		}
		return key, data, nil
	}
	file, err := os.Open(fromFile)
	if err != nil {
		return "", "", fmt.Errorf("failed to open secret value file: %w", err)
	}
	defer file.Close()
	data, err := readSecretInput(file, fromFile)
	if err != nil {
		return "", "", err
	}
	return key, data, nil
}

func readSecretInput(r io.Reader, source string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSecretInputBytes+1))
	if err != nil {
		return "", fmt.Errorf("failed to read secret value from %s: %w", source, err)
	}
	if len(data) > maxSecretInputBytes {
		return "", fmt.Errorf("secret value from %s exceeds %d bytes", source, maxSecretInputBytes)
	}
	return stripOneTrailingNewline(string(data)), nil
}

func stripOneTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\r\n") {
		return value[:len(value)-2]
	}
	return strings.TrimSuffix(value, "\n")
}

// readDotenvSecrets parses a dotenv stream from path, or from stdin when path
// is empty or "-". Parse errors never include values.
func readDotenvSecrets(path string, stdin io.Reader) (map[string]string, error) {
	source := "stdin"
	r := stdin
	if path != "" && path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("failed to open dotenv file: %w", err)
		}
		defer file.Close()
		source = path
		r = file
	}
	data, err := io.ReadAll(io.LimitReader(r, 16*maxSecretInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read dotenv stream from %s: %w", source, err)
	}
	if len(data) > 16*maxSecretInputBytes {
		return nil, fmt.Errorf("dotenv stream from %s exceeds %d bytes", source, 16*maxSecretInputBytes)
	}
	values, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse dotenv stream from %s", source)
	}
	return values, nil
}

// mapDotenvSecrets keeps dotenv keys verbatim, applying only explicit
// SOURCE=DEST mappings, and rejects keys that are not valid env names.
func mapDotenvSecrets(values map[string]string, mappings map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	sources := make(map[string]string, len(values))
	for source, value := range values {
		key := source
		if mapped, ok := mappings[source]; ok {
			key = mapped
		}
		if !secrets.IsValidSecretKey(key) {
			return nil, fmt.Errorf("dotenv key %q is not a valid env key", key)
		}
		if previous, exists := sources[key]; exists {
			return nil, fmt.Errorf("dotenv keys %q and %q both map to %s", previous, source, key)
		}
		sources[key] = source
		out[key] = value
	}
	return out, nil
}
