package takodclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

const DefaultSocket = "/run/tako/takod.sock"

func RequestJSON(client *ssh.Client, socket string, method string, endpoint string, value any) (string, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return "", fmt.Errorf("takod endpoint must start with /")
	}
	if method == "" {
		method = "GET"
	}

	var tmpPath string
	if value != nil {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", err
		}
		data = append(data, '\n')
		tmpPath = fmt.Sprintf("/tmp/tako-takod-%d.json", time.Now().UnixNano())
		if err := client.UploadReader(strings.NewReader(string(data)), tmpPath, 0600); err != nil {
			return "", fmt.Errorf("failed to upload takod request: %w", err)
		}
	}

	args := []string{
		"curl --fail --silent --show-error",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
	}
	if value != nil {
		args = append(args, "-H 'Content-Type: application/json'", "--data-binary @"+shellQuote(tmpPath))
	}
	args = append(args, shellQuote("http://takod"+endpoint))

	curlCmd := strings.Join(args, " ")
	if tmpPath != "" {
		curlCmd = fmt.Sprintf("status=0; if test -S %[1]s && command -v curl >/dev/null 2>&1; then %[2]s || status=$?; else echo 'takod socket or curl is unavailable' >&2; status=42; fi; rm -f %[3]s; exit $status", shellQuote(socket), curlCmd, shellQuote(tmpPath))
	} else {
		curlCmd = fmt.Sprintf("if test -S %[1]s && command -v curl >/dev/null 2>&1; then %[2]s; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi", shellQuote(socket), curlCmd)
	}

	output, err := client.Execute(curlCmd)
	if err != nil {
		return output, fmt.Errorf("takod request %s %s failed: %w, output: %s", method, endpoint, err, output)
	}
	return output, nil
}

func StreamRequest(client *ssh.Client, socket string, method string, endpoint string, reader io.Reader) (string, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return "", fmt.Errorf("takod endpoint must start with /")
	}
	if method == "" {
		method = "POST"
	}

	args := []string{
		"if test -S " + shellQuote(socket) + " && command -v curl >/dev/null 2>&1; then",
		"curl --fail --silent --show-error",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
		"--data-binary @-",
		shellQuote("http://takod" + endpoint),
		"; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi",
	}
	curlCmd := strings.Join(args, " ")
	output, err := client.ExecuteWithInput(context.Background(), curlCmd, reader)
	if err != nil {
		return output, fmt.Errorf("takod stream request %s %s failed: %w, output: %s", method, endpoint, err, output)
	}
	return output, nil
}

func ProxyFileEndpoint(name string) string {
	return "/v1/proxy-file?name=" + url.QueryEscape(name)
}

func StateEndpoint(project string, environment string, document string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("document", document)
	return "/v1/state?" + query.Encode()
}

func EnvBundleEndpoint(project string, environment string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	return "/v1/env-bundle?" + query.Encode()
}

func BackupsEndpoint(project string, environment string, volume string, backupID string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	if volume != "" {
		query.Set("volume", volume)
	}
	if backupID != "" {
		query.Set("backupId", backupID)
	}
	return "/v1/backups?" + query.Encode()
}

func ImageBuildEndpoint(image string) string {
	query := url.Values{}
	query.Set("image", image)
	return "/v1/images/build?" + query.Encode()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
