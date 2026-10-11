package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestFetchAWSSecretsManagerBatchesTwentyNames(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	var names []string
	for i := 0; i < 25; i++ {
		names = append(names, "secret-"+string(rune('a'+i)))
	}
	got, err := FetchProviderSecrets(context.Background(), "aws-sm", names, ProviderOptions{})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d flattened secret(s), want 50", len(got))
	}
	entries := readProviderCommandLog(t, logPath)
	if len(entries) != 2 {
		t.Fatalf("commands = %#v, want two Secrets Manager batches", entries)
	}
	if count := len(strings.Fields(entries[0])) - 6; count != 20 {
		t.Fatalf("first batch carried %d names, want 20: %q", count, entries[0])
	}
}

func TestFetchAWSSecretsManagerByPrefixUsesNameFilter(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "aws-secrets-manager", nil, ProviderOptions{Path: "prod/", Region: "eu-west-1"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if got["prod/api/USER"] != "admin" {
		t.Fatalf("prefix result = %#v", got)
	}
	want := "aws secretsmanager batch-get-secret-value --filters Key=name,Values=prod/ --output json --region eu-west-1"
	if entries := readProviderCommandLog(t, logPath); !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestFetchVaultKVUnwrapsV2AndWalksPaths(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "hashicorp-vault", []string{"legacy-v1"}, ProviderOptions{
		Path: "secret/app/",
		From: "kv",
	})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	want := map[string]string{
		"kv/legacy-v1/TOKEN":         "tok-kv/legacy-v1",
		"secret/app/api/TOKEN":       "tok-secret/app/api",
		"secret/app/api/PORT":        "8080",
		"secret/app/workers/q/TOKEN": "tok-secret/app/workers/q",
		"secret/app/workers/q/PORT":  "8080",
		"kv/legacy-v1/PORT":          "8080",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("got[%s] = %q, want %q (all: %#v)", key, got[key], value, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestFetchOpenBaoUsesBaoCommand(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	if _, err := FetchProviderSecrets(context.Background(), "openbao", []string{"secret/app"}, ProviderOptions{}); err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	want := "bao kv get -format=json secret/app"
	if entries := readProviderCommandLog(t, logPath); !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestFetchOnePasswordItemsAndReferences(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "1password", []string{"Postgres", "op://Prod/Stripe/api-key"}, ProviderOptions{Vault: "Prod"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if got["Postgres/password"] != "pw-Postgres" || got["Prod/Stripe/api-key"] != "ref-op://Prod/Stripe/api-key" {
		t.Fatalf("1Password result = %#v", got)
	}
	if _, ok := got["Postgres/notesPlain"]; ok {
		t.Fatalf("empty field should be skipped: %#v", got)
	}

	entries := readProviderCommandLog(t, logPath)
	for _, want := range []string{
		"op item get Postgres --format json --vault Prod",
		"op read --no-newline op://Prod/Stripe/api-key",
	} {
		if !slices.Contains(entries, want) {
			t.Fatalf("commands = %#v, want %q", entries, want)
		}
	}
}

func TestFetchOnePasswordWholeVault(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "op", nil, ProviderOptions{Vault: "Prod"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if got["item-1/password"] != "pw-item-1" || got["item-2/password"] != "pw-item-2" {
		t.Fatalf("vault result = %#v", got)
	}
	if _, err := FetchProviderSecrets(context.Background(), "op", nil, ProviderOptions{}); err == nil {
		t.Fatal("expected an error without names or --vault")
	}
}

func TestFetchDopplerDropsMetadataAndSelectsNames(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	all, err := FetchProviderSecrets(context.Background(), "doppler", nil, ProviderOptions{Project: "api", ProviderEnv: "prd"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if len(all) != 2 || all["API_KEY"] != "doppler-api" {
		t.Fatalf("doppler result = %#v", all)
	}
	want := "doppler secrets download --no-file --format json --project api --config prd"
	if entries := readProviderCommandLog(t, logPath); !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}

	selected, err := FetchProviderSecrets(context.Background(), "doppler", []string{"DATABASE_URL"}, ProviderOptions{})
	if err != nil || len(selected) != 1 || selected["DATABASE_URL"] != "doppler-db" {
		t.Fatalf("selected = %#v, err = %v", selected, err)
	}
	if _, err := FetchProviderSecrets(context.Background(), "doppler", []string{"NOPE"}, ProviderOptions{}); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("missing-name error = %v", err)
	}
}

func TestFetchInfisicalPassesProjectEnvAndPath(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "infisical", nil, ProviderOptions{Project: "p-1", ProviderEnv: "prod", Path: "/api"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if got["API_KEY"] != "infisical-api" {
		t.Fatalf("infisical result = %#v", got)
	}
	want := "infisical export --format=json --env=prod --projectId=p-1 --path=/api"
	if entries := readProviderCommandLog(t, logPath); !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestFetchBitwardenScopesProject(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "bws", []string{"API_KEY"}, ProviderOptions{Project: "proj-uuid"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if len(got) != 1 || got["API_KEY"] != "bws-api" {
		t.Fatalf("bitwarden result = %#v", got)
	}
	want := "bws secret list proj-uuid --output json"
	if entries := readProviderCommandLog(t, logPath); !slices.Contains(entries, want) {
		t.Fatalf("commands = %#v, want %q", entries, want)
	}
}

func TestFetchGCPSecretManagerDecodesPayloadAndListsPrefix(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	got, err := FetchProviderSecrets(context.Background(), "gcp", nil, ProviderOptions{Project: "acme", Path: "app-"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if len(got) != 1 || got["app-db"] != "gcp-app-db\n" {
		t.Fatalf("gcp result = %#v", got)
	}
	entries := readProviderCommandLog(t, logPath)
	for _, want := range []string{
		"gcloud secrets list --format=json --project=acme",
		"gcloud secrets versions access latest --secret=app-db --format=json --project=acme",
	} {
		if !slices.Contains(entries, want) {
			t.Fatalf("commands = %#v, want %q", entries, want)
		}
	}
}

func TestFetchAzureKeyVaultRequiresVaultAndSkipsDisabled(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeProviderCommand(t, logPath)
	defer restore()

	if _, err := FetchProviderSecrets(context.Background(), "azure-key-vault", []string{"x"}, ProviderOptions{}); err == nil {
		t.Fatal("expected --vault to be required")
	}
	got, err := FetchProviderSecrets(context.Background(), "akv", nil, ProviderOptions{Vault: "acme-kv", Path: "app-"})
	if err != nil {
		t.Fatalf("FetchProviderSecrets returned error: %v", err)
	}
	if len(got) != 1 || got["app-db"] != "az-app-db" {
		t.Fatalf("azure result = %#v", got)
	}
}

func TestFetchProviderSecretsReportsMissingCLIAndUnknownProvider(t *testing.T) {
	oldLookPath := providerLookPath
	providerLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	defer func() { providerLookPath = oldLookPath }()

	_, err := FetchProviderSecrets(context.Background(), "doppler", nil, ProviderOptions{})
	if err == nil || !strings.Contains(err.Error(), `"doppler" CLI`) || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("missing CLI error = %v", err)
	}
	_, err = FetchProviderSecrets(context.Background(), "keepass", nil, ProviderOptions{})
	if err == nil || !strings.Contains(err.Error(), "aws-ssm") {
		t.Fatalf("unknown provider error = %v", err)
	}
}

func TestProviderAliasesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, info := range Providers() {
		for _, name := range append([]string{info.Name}, info.Aliases...) {
			if owner, ok := seen[name]; ok {
				t.Fatalf("%q is claimed by both %s and %s", name, owner, info.Name)
			}
			seen[name] = info.Name
		}
		if info.Command == "" || info.Install == "" || info.Description == "" {
			t.Fatalf("provider %s is missing metadata: %#v", info.Name, info)
		}
	}
}

func useFakeProviderCommand(t *testing.T, logPath string) func() {
	t.Helper()
	oldCommand := providerCommandContext
	oldLookPath := providerLookPath
	providerCommandContext = fakeProviderCommandContext
	providerLookPath = func(name string) (string, error) { return name, nil }
	t.Setenv("GO_WANT_TAKO_PROVIDER_HELPER", "1")
	t.Setenv("TAKO_PROVIDER_COMMAND_LOG", logPath)
	return func() {
		providerCommandContext = oldCommand
		providerLookPath = oldLookPath
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
	if len(commandArgs) < 1 {
		os.Exit(2)
	}
	sub := commandArgs[0]
	if len(commandArgs) > 1 {
		sub += " " + commandArgs[1]
	}

	switch command + " " + sub {
	case "aws ssm get-parameters":
		writeFakeSSMGetParameters(commandArgs)
	case "aws ssm get-parameters-by-path":
		writeFakeSSMGetParametersByPath(commandArgs)
	case "aws secretsmanager batch-get-secret-value":
		writeFakeSecretsManager(commandArgs)
	case "vault kv get", "bao kv get":
		writeFakeVaultGet(commandArgs[len(commandArgs)-1])
	case "vault kv list", "bao kv list":
		writeFakeVaultList(commandArgs[len(commandArgs)-1])
	case "op item get":
		name := commandArgs[2]
		writeJSON(map[string]any{
			"title": name,
			"fields": []map[string]any{
				{"id": "password", "label": "password", "value": "pw-" + name},
				{"id": "notesPlain", "label": "notesPlain", "value": ""},
				{"id": "untouched", "label": "untouched"},
			},
		})
	case "op item list":
		writeJSON([]map[string]string{{"id": "item-1", "title": "One"}, {"id": "item-2", "title": "Two"}})
	case "op read --no-newline":
		_, _ = os.Stdout.WriteString("ref-" + commandArgs[2])
	case "doppler secrets download":
		writeJSON(map[string]string{
			"API_KEY":             "doppler-api",
			"DATABASE_URL":        "doppler-db",
			"DOPPLER_PROJECT":     "api",
			"DOPPLER_CONFIG":      "prd",
			"DOPPLER_ENVIRONMENT": "prd",
		})
	case "infisical export --format=json":
		writeJSON([]map[string]string{{"key": "API_KEY", "value": "infisical-api"}, {"key": "DB", "value": "infisical-db"}})
	case "bws secret list":
		writeJSON([]map[string]string{{"key": "API_KEY", "value": "bws-api"}, {"key": "OTHER", "value": "bws-other"}})
	case "gcloud secrets list":
		writeJSON([]map[string]string{{"name": "projects/acme/secrets/app-db"}, {"name": "projects/acme/secrets/other"}})
	case "gcloud secrets versions":
		name := strings.TrimPrefix(fakeFlagValue(commandArgs, "--secret="), "--secret=")
		writeJSON(map[string]any{"payload": map[string]string{
			"data": base64.StdEncoding.EncodeToString([]byte("gcp-" + name + "\n")),
		}})
	case "az keyvault secret":
		writeFakeAzure(commandArgs)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func fakeFlagValue(args []string, prefix string) string {
	for i, arg := range args {
		if arg == prefix {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, prefix) {
			return arg
		}
	}
	return ""
}

func fakeFlagValues(args []string, flag string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		if args[i] != flag {
			continue
		}
		for j := i + 1; j < len(args); j++ {
			if strings.HasPrefix(args[j], "--") {
				break
			}
			values = append(values, args[j])
		}
		break
	}
	return values
}

func writeFakeSSMGetParameters(args []string) {
	names := fakeFlagValues(args, "--names")
	parameters := make([]map[string]string, 0, len(names))
	for _, name := range names {
		parameters = append(parameters, map[string]string{"Name": name, "Value": "value-" + name})
	}
	writeJSON(map[string]any{"Parameters": parameters, "InvalidParameters": []string{}})
}

func writeFakeSSMGetParametersByPath(args []string) {
	path := fakeFlagValue(args, "--path")
	name := strings.TrimRight(path, "/") + "/database-url"
	writeJSON(map[string]any{
		"Parameters": []map[string]string{{"Name": name, "Value": "value-" + name}},
	})
}

func writeFakeSecretsManager(args []string) {
	names := fakeFlagValues(args, "--secret-id-list")
	if filter := fakeFlagValue(args, "--filters"); filter != "" {
		names = []string{strings.TrimPrefix(filter, "Key=name,Values=") + "api"}
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

func writeFakeVaultGet(path string) {
	fields := map[string]any{"TOKEN": "tok-" + path, "PORT": 8080}
	if strings.Contains(path, "v1") {
		writeJSON(map[string]any{"data": fields})
		return
	}
	writeJSON(map[string]any{"data": map[string]any{"data": fields, "metadata": map[string]any{"version": 3}}})
}

func writeFakeVaultList(path string) {
	switch path {
	case "secret/app":
		writeJSON([]string{"api", "workers/"})
	case "secret/app/workers":
		writeJSON([]string{"q"})
	default:
		_, _ = os.Stderr.WriteString("No value found at " + path)
		os.Exit(2)
	}
}

func writeFakeAzure(args []string) {
	switch args[2] {
	case "list":
		writeJSON([]map[string]any{
			{"name": "app-db", "attributes": map[string]any{"enabled": true}},
			{"name": "app-old", "attributes": map[string]any{"enabled": false}},
			{"name": "other", "attributes": map[string]any{"enabled": true}},
		})
	case "show":
		writeJSON(map[string]string{"value": "az-" + fakeFlagValue(args, "--name")})
	default:
		os.Exit(2)
	}
}

func writeJSON(value any) {
	encoded, _ := json.Marshal(value)
	_, _ = os.Stdout.Write(encoded)
}

func readProviderCommandLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}
