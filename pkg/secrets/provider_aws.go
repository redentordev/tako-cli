package secrets

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	awsSSMMaxNamesPerRequest            = 10
	awsSecretsManagerMaxNamesPerRequest = 20
)

type awsSSMParameter struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type awsSecretValue struct {
	Name         string `json:"Name"`
	SecretString string `json:"SecretString"`
	SecretBinary string `json:"SecretBinary"`
}

type awsSecretError struct {
	SecretID  string `json:"SecretId"`
	ErrorCode string `json:"ErrorCode"`
	Message   string `json:"Message"`
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
		var response struct {
			Parameters        []awsSSMParameter `json:"Parameters"`
			InvalidParameters []string          `json:"InvalidParameters"`
		}
		if err := runProviderJSON(ctx, &response, "AWS SSM", "aws", args...); err != nil {
			return nil, err
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
	startingToken := ""
	for {
		args := []string{
			"ssm", "get-parameters-by-path",
			"--path", options.Path,
			"--with-decryption",
			"--recursive",
			"--output", "json",
		}
		// The AWS CLI paginates automatically; NextToken only appears when
		// a page cap truncates output, and resumes through --starting-token.
		if startingToken != "" {
			args = append(args, "--starting-token", startingToken)
		}
		args = appendAWSFlags(args, options)
		var response struct {
			Parameters []awsSSMParameter `json:"Parameters"`
			NextToken  string            `json:"NextToken"`
		}
		if err := runProviderJSON(ctx, &response, "AWS SSM", "aws", args...); err != nil {
			return nil, err
		}
		for _, parameter := range response.Parameters {
			results[parameter.Name] = parameter.Value
		}
		if response.NextToken == "" || response.NextToken == startingToken {
			break
		}
		startingToken = response.NextToken
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no SSM parameters found under %s", options.Path)
	}
	return results, nil
}

func fetchAWSSecretsManagerSecrets(ctx context.Context, names []string, options ProviderOptions) (map[string]string, error) {
	if strings.TrimSpace(options.Path) != "" {
		return fetchAWSSecretsManagerByPrefix(ctx, options)
	}
	names = prefixedSecretNames(names, options.From)
	if len(names) == 0 {
		return nil, fmt.Errorf("provide AWS Secrets Manager secret names or --path")
	}

	results := make(map[string]string)
	for _, batch := range stringBatches(names, awsSecretsManagerMaxNamesPerRequest) {
		args := []string{"secretsmanager", "batch-get-secret-value", "--output", "json", "--secret-id-list"}
		args = append(args, batch...)
		args = appendAWSFlags(args, options)
		var response struct {
			SecretValues []awsSecretValue `json:"SecretValues"`
			Errors       []awsSecretError `json:"Errors"`
		}
		if err := runProviderJSON(ctx, &response, "AWS Secrets Manager", "aws", args...); err != nil {
			return nil, err
		}
		if err := awsSecretErrors(response.Errors); err != nil {
			return nil, err
		}
		for _, secret := range response.SecretValues {
			storeAWSSecretValue(results, secret)
		}
	}
	return results, nil
}

func fetchAWSSecretsManagerByPrefix(ctx context.Context, options ProviderOptions) (map[string]string, error) {
	results := make(map[string]string)
	startingToken := ""
	for {
		args := []string{
			"secretsmanager", "batch-get-secret-value",
			"--filters", "Key=name,Values=" + options.Path,
			"--output", "json",
		}
		if startingToken != "" {
			args = append(args, "--starting-token", startingToken)
		}
		args = appendAWSFlags(args, options)
		var response struct {
			SecretValues []awsSecretValue `json:"SecretValues"`
			Errors       []awsSecretError `json:"Errors"`
			NextToken    string           `json:"NextToken"`
		}
		if err := runProviderJSON(ctx, &response, "AWS Secrets Manager", "aws", args...); err != nil {
			return nil, err
		}
		if err := awsSecretErrors(response.Errors); err != nil {
			return nil, err
		}
		for _, secret := range response.SecretValues {
			storeAWSSecretValue(results, secret)
		}
		if response.NextToken == "" || response.NextToken == startingToken {
			break
		}
		startingToken = response.NextToken
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no AWS Secrets Manager secrets found with name prefix %s", options.Path)
	}
	return results, nil
}

func storeAWSSecretValue(results map[string]string, secret awsSecretValue) {
	if secret.SecretString == "" && secret.SecretBinary != "" {
		// Binary secrets arrive base64-encoded; keep that encoding.
		results[secret.Name] = secret.SecretBinary
		return
	}
	flattenSecretValue(results, secret.Name, secret.SecretString)
}

func awsSecretErrors(items []awsSecretError) error {
	if len(items) == 0 {
		return nil
	}
	messages := make([]string, 0, len(items))
	for _, item := range items {
		message := item.Message
		if message == "" {
			message = item.ErrorCode
		}
		messages = append(messages, fmt.Sprintf("%s: %s", item.SecretID, message))
	}
	sort.Strings(messages)
	return fmt.Errorf("failed to read AWS secret(s): %s", strings.Join(messages, "; "))
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
