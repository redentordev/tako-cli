package secrets

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFetchAWSSSMSecretsBatchesNamesAndUsesProfileRegion(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	names := []string{"ONE", "TWO", "THREE", "FOUR", "FIVE", "SIX", "SEVEN", "EIGHT", "NINE", "TEN", "ELEVEN"}
	got, err := FetchProviderSecrets(context.Background(), "aws-ssm", names, ProviderOptions{
		Profile: "truenextglobal",
		Region:  "us-west-1",
		From:    "/app/prod",
	})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("got %d secret(s), want %d: %#v", len(got), len(names), got)
	}
	if got["/app/prod/ONE"] != "value-/app/prod/ONE" {
		t.Fatalf("prefixed SSM value not returned: %#v", got)
	}

	entries := readProviderCommandLog(t, logPath)
	if len(entries) != 2 {
		t.Fatalf("commands = %#v, want two SSM batches", entries)
	}
	first := "aws ssm get-parameters --with-decryption --output json --names /app/prod/ONE /app/prod/TWO /app/prod/THREE /app/prod/FOUR /app/prod/FIVE /app/prod/SIX /app/prod/SEVEN /app/prod/EIGHT /app/prod/NINE /app/prod/TEN --profile truenextglobal --region us-west-1"
	second := "aws ssm get-parameters --with-decryption --output json --names /app/prod/ELEVEN --profile truenextglobal --region us-west-1"
	if entries[0] != first || entries[1] != second {
		t.Fatalf("commands = %#v, want %#v", entries, []string{first, second})
	}
}

func TestFetchAWSSSMSecretsByPathUsesRecursiveDecryption(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "ssm", nil, ProviderOptions{
		Path: "/truenextglobal/production",
	})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if got["/truenextglobal/production/database-url"] != "value-/truenextglobal/production/database-url" {
		t.Fatalf("path result = %#v", got)
	}

	entries := readProviderCommandLog(t, logPath)
	want := "aws ssm get-parameters-by-path --path /truenextglobal/production --with-decryption --recursive --output json"
	if !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestFetchAWSSecretsManagerFlattensJSONString(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "aws-secrets-manager", []string{"app"}, ProviderOptions{})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if got["app/USER"] != "admin" || got["app/PORT"] != "5432" {
		t.Fatalf("flattened secrets = %#v", got)
	}
}

func useFakeProviderCommand(t *testing.T, logPath string) func() {
	t.Helper()
	oldCommand := providerCommandContext
	providerCommandContext = fakeProviderCommandContext
	t.Setenv("GO_WANT_TAKO_PROVIDER_HELPER", "1")
	t.Setenv("TAKO_PROVIDER_COMMAND_LOG", logPath)
	return func() {
		providerCommandContext = oldCommand
	}
}

func fakeProviderCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=TestProviderCommandHelper", "--", name}, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestProviderCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_TAKO_PROVIDER_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || separator+1 >= len(args) {
		os.Exit(2)
	}
	command := args[separator+1]
	commandArgs := args[separator+2:]
	entry := strings.Join(append([]string{command}, commandArgs...), " ")
	if logPath := os.Getenv("TAKO_PROVIDER_COMMAND_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = file.WriteString(entry + "\n")
		_ = file.Close()
	}
	if command != "aws" || len(commandArgs) < 2 {
		os.Exit(2)
	}

	switch commandArgs[0] + " " + commandArgs[1] {
	case "ssm get-parameters":
		writeFakeSSMGetParameters(commandArgs)
	case "ssm get-parameters-by-path":
		writeFakeSSMGetParametersByPath(commandArgs)
	case "secretsmanager batch-get-secret-value":
		writeFakeSecretsManager(commandArgs)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func writeFakeSSMGetParameters(args []string) {
	var names []string
	for i := 0; i < len(args); i++ {
		if args[i] != "--names" {
			continue
		}
		for j := i + 1; j < len(args); j++ {
			if strings.HasPrefix(args[j], "--") {
				break
			}
			names = append(names, args[j])
		}
		break
	}
	parameters := make([]map[string]string, 0, len(names))
	for _, name := range names {
		parameters = append(parameters, map[string]string{"Name": name, "Value": "value-" + name})
	}
	writeJSON(map[string]any{"Parameters": parameters, "InvalidParameters": []string{}})
}

func writeFakeSSMGetParametersByPath(args []string) {
	path := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--path" {
			path = args[i+1]
			break
		}
	}
	name := strings.TrimRight(path, "/") + "/database-url"
	writeJSON(map[string]any{
		"Parameters": []map[string]string{{"Name": name, "Value": "value-" + name}},
	})
}

func writeFakeSecretsManager(args []string) {
	var names []string
	for i := 0; i < len(args); i++ {
		if args[i] != "--secret-id-list" {
			continue
		}
		for j := i + 1; j < len(args); j++ {
			if strings.HasPrefix(args[j], "--") {
				break
			}
			names = append(names, args[j])
		}
		break
	}
	values := make([]map[string]string, 0, len(names))
	for _, name := range names {
		values = append(values, map[string]string{
			"Name":         name,
			"SecretString": `{"USER":"admin","PORT":5432}`,
		})
	}
	writeJSON(map[string]any{"SecretValues": values})
}

func writeJSON(value any) {
	encoded, _ := json.Marshal(value)
	_, _ = os.Stdout.Write(encoded)
}

func readProviderCommandLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}
