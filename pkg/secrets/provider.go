package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	ProviderAWSSSM            = "aws-ssm"
	ProviderAWSSecretsManager = "aws-secrets-manager"

	awsSSMMaxNamesPerRequest = 10
)

var (
	providerCommandContext = exec.CommandContext
	providerCommandTimeout = 2 * time.Minute
)

type ProviderOptions struct {
	Profile string
	Region  string
	Path    string
	From    string
}

func FetchProviderSecrets(ctx context.Context, provider string, names []string, options ProviderOptions) (map[string]string, error) {
	switch normalizeProvider(provider) {
	case ProviderAWSSSM:
		return fetchAWSSSMSecrets(ctx, names, options)
	case ProviderAWSSecretsManager:
		return fetchAWSSecretsManagerSecrets(ctx, names, options)
	default:
		return nil, fmt.Errorf("unsupported secrets provider %q", provider)
	}
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aws-ssm", "ssm", "aws_ssm", "aws-ssm-parameter-store", "aws_ssm_parameter_store":
		return ProviderAWSSSM
	case "aws-secrets-manager", "aws_secrets_manager", "secretsmanager", "aws-sm":
		return ProviderAWSSecretsManager
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func fetchAWSSSMSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	if strings.TrimSpace(options.Path) != "" {
		return fetchAWSSSMSecretsByPath(ctx, options)
	}
	names = prefixedSecretNames(names, options.From)
	if len(names) == 0 {
		return nil, fmt.Errorf("provide SSM parameter names or --path")
	}

	results := make(map[string]string)
	for _, batch := range stringBatches(names, awsSSMMaxNamesPerRequest) {
		args := []string{"ssm", "get-parameters", "--with-decryption", "--output", "json", "--names"}
		args = append(args, batch...)
		args = appendAWSFlags(args, options)
		output, err := runProviderCommand(ctx, "aws", args...)
		if err != nil {
			return nil, err
		}
		var response struct {
			Parameters []struct {
				Name  string `json:"Name"`
				Value string `json:"Value"`
			} `json:"Parameters"`
			InvalidParameters []string `json:"InvalidParameters"`
		}
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse AWS SSM response: %w", err)
		}
		if len(response.InvalidParameters) > 0 {
			sort.Strings(response.InvalidParameters)
			return nil, fmt.Errorf("missing SSM parameter(s): %s", strings.Join(response.InvalidParameters, ", "))
		}
		for _, parameter := range response.Parameters {
			results[parameter.Name] = parameter.Value
		}
	}
	return results, nil
}

func fetchAWSSSMSecretsByPath(ctx context.Context, options ProviderOptions) (map[string]string, error) {
	results := make(map[string]string)
	nextToken := ""
	for {
		args := []string{
			"ssm", "get-parameters-by-path",
			"--path", options.Path,
			"--with-decryption",
			"--recursive",
			"--output", "json",
		}
		if nextToken != "" {
			args = append(args, "--next-token", nextToken)
		}
		args = appendAWSFlags(args, options)
		output, err := runProviderCommand(ctx, "aws", args...)
		if err != nil {
			return nil, err
		}
		var response struct {
			Parameters []struct {
				Name  string `json:"Name"`
				Value string `json:"Value"`
			} `json:"Parameters"`
			NextToken string `json:"NextToken"`
		}
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, fmt.Errorf("failed to parse AWS SSM response: %w", err)
		}
		for _, parameter := range response.Parameters {
			results[parameter.Name] = parameter.Value
		}
		if response.NextToken == "" {
			break
		}
		nextToken = response.NextToken
	}
	return results, nil
}

func fetchAWSSecretsManagerSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	names = prefixedSecretNames(names, options.From)
	if len(names) == 0 {
		return nil, fmt.Errorf("provide AWS Secrets Manager secret names")
	}

	args := []string{"secretsmanager", "batch-get-secret-value", "--output", "json", "--secret-id-list"}
	args = append(args, names...)
	args = appendAWSFlags(args, options)
	output, err := runProviderCommand(ctx, "aws", args...)
	if err != nil {
		return nil, err
	}
	var response struct {
		SecretValues []struct {
			Name         string `json:"Name"`
			SecretString string `json:"SecretString"`
		} `json:"SecretValues"`
		Errors []struct {
			SecretID string `json:"SecretId"`
			Message  string `json:"Message"`
		} `json:"Errors"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("failed to parse AWS Secrets Manager response: %w", err)
	}
	if len(response.Errors) > 0 {
		messages := make([]string, 0, len(response.Errors))
		for _, item := range response.Errors {
			messages = append(messages, fmt.Sprintf("%s: %s", item.SecretID, item.Message))
		}
		sort.Strings(messages)
		return nil, fmt.Errorf("failed to read AWS secret(s): %s", strings.Join(messages, "; "))
	}

	results := make(map[string]string)
	for _, secret := range response.SecretValues {
		flattenSecretValue(results, secret.Name, secret.SecretString)
	}
	return results, nil
}

func flattenSecretValue(results map[string]string, name string, value string) {
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		results[name] = value
		return
	}
	for key, item := range object {
		resultKey := name + "/" + key
		if str, ok := item.(string); ok {
			results[resultKey] = str
			continue
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			results[resultKey] = fmt.Sprint(item)
			continue
		}
		results[resultKey] = string(encoded)
	}
}

func appendAWSFlags(args []string, options ProviderOptions) []string {
	if options.Profile != "" {
		args = append(args, "--profile", options.Profile)
	}
	if options.Region != "" {
		args = append(args, "--region", options.Region)
	}
	return args
}

func prefixedSecretNames(names []string, from string) []string {
	out := make([]string, 0, len(names))
	from = strings.TrimRight(strings.TrimSpace(from), "/")
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if from != "" {
			name = from + "/" + strings.TrimLeft(name, "/")
		}
		out = append(out, name)
	}
	return out
}

func stringBatches(values []string, size int) [][]string {
	if size <= 0 || len(values) == 0 {
		return nil
	}
	var batches [][]string
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		batches = append(batches, values[start:end])
	}
	return batches
}

func runProviderCommand(ctx context.Context, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, providerCommandTimeout)
	defer cancel()
	cmd := providerCommandContext(commandCtx, name, args...)
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if commandCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s command timed out after %s", name, providerCommandTimeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			message := strings.TrimSpace(string(exitErr.Stderr))
			if message == "" {
				message = strings.TrimSpace(string(output))
			}
			if message != "" {
				return "", fmt.Errorf("%s failed: %s", name, message)
			}
		}
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	return string(output), nil
}
