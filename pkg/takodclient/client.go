package takodclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultSocket       = "/run/tako/takod.sock"
	DefaultWorkerSocket = "/run/tako-platform/worker.sock"
)

const (
	JSONRequestTimeout   = 2 * time.Minute
	StreamRequestTimeout = 30 * time.Minute
	httpStatusMarker     = "\n__TAKO_HTTP_STATUS__:"
)

type RequestExecutor interface {
	ExecuteWithContext(ctx context.Context, cmd string) (string, error)
	ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error)
}

type StreamExecutor interface {
	RequestExecutor
	ExecuteStream(cmd string, stdout io.Writer, stderr io.Writer) error
}

type structuredJSONClient interface {
	RequestJSON(context.Context, string, string, any) (string, error)
	RequestJSONWithTimeout(context.Context, string, string, any, time.Duration) (string, error)
}

type structuredStreamClient interface {
	StreamRequest(context.Context, string, string, io.Reader, string) (string, error)
	StreamOutput(context.Context, string, string, io.Reader, string, io.Writer, io.Writer) error
}

type contextStreamExecutor interface {
	ExecuteStreamWithContext(ctx context.Context, cmd string, stdout io.Writer, stderr io.Writer) error
}

// HTTPError preserves a non-success takod response status so command adapters
// can map rejected input to their typed machine error taxonomy.
type HTTPError struct {
	Method   string
	Endpoint string
	Status   int
	Body     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("takod request %s %s returned HTTP %d: %s", e.Method, e.Endpoint, e.Status, strings.TrimSpace(e.Body))
}

// HTTPStatus allows durable operation adapters to distinguish a known
// upstream HTTP rejection from an uncertain transport failure.
func (e *HTTPError) HTTPStatus() int { return e.Status }

func RequestJSON(client any, socket string, method string, endpoint string, value any) (string, error) {
	return RequestJSONWithContext(context.Background(), client, socket, method, endpoint, value)
}

func RequestJSONWithContext(ctx context.Context, client any, socket string, method string, endpoint string, value any) (string, error) {
	return RequestJSONWithTimeoutContext(ctx, client, socket, method, endpoint, value, JSONRequestTimeout)
}

func RequestJSONWithTimeout(client any, socket string, method string, endpoint string, value any, timeout time.Duration) (string, error) {
	return RequestJSONWithTimeoutContext(context.Background(), client, socket, method, endpoint, value, timeout)
}

func RequestJSONWithTimeoutContext(ctx context.Context, client any, socket string, method string, endpoint string, value any, timeout time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return "", fmt.Errorf("takod endpoint must start with /")
	}
	if method == "" {
		method = "GET"
	}
	if timeout <= 0 {
		timeout = JSONRequestTimeout
	}
	if structured, ok := client.(structuredJSONClient); ok {
		return structured.RequestJSONWithTimeout(ctx, method, endpoint, value, timeout)
	}
	if dialer, ok := client.(UnixSocketDialer); ok {
		structured, err := NewAgentClient(dialer, socket)
		if err != nil {
			return "", err
		}
		defer structured.CloseIdleConnections()
		return structured.RequestJSONWithTimeout(ctx, method, endpoint, value, timeout)
	}
	executor, ok := client.(RequestExecutor)
	if !ok {
		return "", fmt.Errorf("takod client does not support structured JSON requests")
	}

	var body io.Reader
	hasBody := value != nil
	if value != nil {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", err
		}
		data = append(data, '\n')
		body = strings.NewReader(string(data))
	}

	curlCmd := buildRequestCommand(socket, method, endpoint, hasBody)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var output string
	var err error
	if hasBody {
		output, err = executor.ExecuteWithInput(ctx, curlCmd, body)
	} else {
		output, err = executor.ExecuteWithContext(ctx, curlCmd)
	}
	bodyOutput, status, hasStatus := splitHTTPStatus(output)
	if err != nil {
		return bodyOutput, fmt.Errorf("takod request %s %s failed: %w, output: %s", method, endpoint, err, bodyOutput)
	}
	bodyOutput = sanitizeJSONOutput(bodyOutput)
	if hasStatus && status >= 400 {
		return bodyOutput, &HTTPError{Method: method, Endpoint: endpoint, Status: status, Body: bodyOutput}
	}
	return bodyOutput, nil
}

func sanitizeJSONOutput(output string) string {
	return strings.TrimLeft(output, "\x00")
}

func buildRequestCommand(socket string, method string, endpoint string, hasBody bool) string {
	args := []string{
		"curl --silent --show-error",
		"--write-out '\\n__TAKO_HTTP_STATUS__:%{http_code}'",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
	}
	if hasBody {
		args = append(args, "-H 'Content-Type: application/json'", "--data-binary @-")
	}
	args = append(args, shellQuote("http://takod"+endpoint))

	curlCmd := strings.Join(args, " ")
	return fmt.Sprintf("if test -S %[1]s && command -v curl >/dev/null 2>&1; then %[2]s; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi", shellQuote(socket), curlCmd)
}

func splitHTTPStatus(output string) (string, int, bool) {
	idx := strings.LastIndex(output, httpStatusMarker)
	if idx < 0 {
		return output, 0, false
	}
	statusText := strings.TrimSpace(output[idx+len(httpStatusMarker):])
	status, err := strconv.Atoi(statusText)
	if err != nil {
		return output, 0, false
	}
	return output[:idx], status, true
}

func StreamRequest(client any, socket string, method string, endpoint string, reader io.Reader) (string, error) {
	return StreamRequestWithContext(context.Background(), client, socket, method, endpoint, reader)
}

func StreamRequestWithContext(ctx context.Context, client any, socket string, method string, endpoint string, reader io.Reader) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return "", fmt.Errorf("takod endpoint must start with /")
	}
	if method == "" {
		method = "POST"
	}
	if structured, ok := client.(structuredStreamClient); ok {
		return structured.StreamRequest(ctx, method, endpoint, reader, "application/octet-stream")
	}
	if dialer, ok := client.(UnixSocketDialer); ok {
		structured, err := NewAgentClient(dialer, socket)
		if err != nil {
			return "", err
		}
		defer structured.CloseIdleConnections()
		return structured.StreamRequest(ctx, method, endpoint, reader, "application/octet-stream")
	}
	executor, ok := client.(RequestExecutor)
	if !ok {
		return "", fmt.Errorf("takod client does not support streamed requests")
	}

	curlCmd := buildStreamRequestCommand(socket, method, endpoint)
	ctx, cancel := context.WithTimeout(ctx, StreamRequestTimeout)
	defer cancel()

	output, err := executor.ExecuteWithInput(ctx, curlCmd, reader)
	bodyOutput, status, hasStatus := splitHTTPStatus(output)
	if err != nil {
		return bodyOutput, fmt.Errorf("takod stream request %s %s failed: %w, output: %s", method, endpoint, err, strings.TrimSpace(bodyOutput))
	}
	bodyOutput = sanitizeJSONOutput(bodyOutput)
	if hasStatus && status >= 400 {
		return bodyOutput, fmt.Errorf("takod stream request %s %s returned HTTP %d: %s", method, endpoint, status, strings.TrimSpace(bodyOutput))
	}
	return bodyOutput, nil
}

func buildStreamRequestCommand(socket string, method string, endpoint string) string {
	args := []string{
		"if test -S " + shellQuote(socket) + " && command -v curl >/dev/null 2>&1; then",
		"curl --silent --show-error",
		"--write-out '\\n__TAKO_HTTP_STATUS__:%{http_code}'",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
		"--data-binary @-",
		shellQuote("http://takod" + endpoint),
		"; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi",
	}
	return strings.Join(args, " ")
}

func StreamOutput(client any, socket string, endpoint string, stdout io.Writer, stderr io.Writer) error {
	return StreamOutputWithContext(context.Background(), client, socket, endpoint, stdout, stderr)
}

func StreamOutputWithContext(ctx context.Context, client any, socket string, endpoint string, stdout io.Writer, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return fmt.Errorf("takod endpoint must start with /")
	}
	if structured, ok := client.(structuredStreamClient); ok {
		return structured.StreamOutput(ctx, "GET", endpoint, nil, "", stdout, stderr)
	}
	if dialer, ok := client.(UnixSocketDialer); ok {
		structured, err := NewAgentClient(dialer, socket)
		if err != nil {
			return err
		}
		defer structured.CloseIdleConnections()
		return structured.StreamOutput(ctx, "GET", endpoint, nil, "", stdout, stderr)
	}
	executor, ok := client.(StreamExecutor)
	if !ok {
		return fmt.Errorf("takod client does not support streamed output")
	}

	curlCmd := buildStreamOutputCommand(socket, endpoint)
	var err error
	if contextClient, ok := any(executor).(contextStreamExecutor); ok {
		err = contextClient.ExecuteStreamWithContext(ctx, curlCmd, stdout, stderr)
	} else {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = executor.ExecuteStream(curlCmd, stdout, stderr)
	}
	if err != nil {
		return fmt.Errorf("takod stream request %s failed: %w", endpoint, err)
	}
	return nil
}

func buildStreamOutputCommand(socket string, endpoint string) string {
	args := []string{
		"if test -S " + shellQuote(socket) + " && command -v curl >/dev/null 2>&1; then",
		"curl --fail --silent --show-error --no-buffer",
		"--unix-socket " + shellQuote(socket),
		shellQuote("http://takod" + endpoint),
		"; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi",
	}
	return strings.Join(args, " ")
}

// inputStreamExecutor is satisfied by clients that can stream a command's
// live output while feeding its stdin (pkg/ssh.Client).
type inputStreamExecutor interface {
	ExecuteStreamWithInput(ctx context.Context, cmd string, input io.Reader, stdout io.Writer, stderr io.Writer) error
}

// StreamRequestOutputWithContext sends a request body and streams the
// chunked response live to stdout/stderr (no buffering, no status marker) —
// the POST counterpart of StreamOutputWithContext. Clients without stdin
// streaming get the body embedded base64-encoded in the remote command.
func StreamRequestOutputWithContext(ctx context.Context, client any, socket string, method string, endpoint string, body io.Reader, stdout io.Writer, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return fmt.Errorf("takod endpoint must start with /")
	}
	if method == "" {
		method = "POST"
	}
	if structured, ok := client.(structuredStreamClient); ok {
		return structured.StreamOutput(ctx, method, endpoint, body, "application/json", stdout, stderr)
	}
	if dialer, ok := client.(UnixSocketDialer); ok {
		structured, err := NewAgentClient(dialer, socket)
		if err != nil {
			return err
		}
		defer structured.CloseIdleConnections()
		return structured.StreamOutput(ctx, method, endpoint, body, "application/json", stdout, stderr)
	}
	executor, ok := client.(StreamExecutor)
	if !ok {
		return fmt.Errorf("takod client does not support streamed request output")
	}

	curlCmd := buildStreamRequestOutputCommand(socket, method, endpoint)
	var err error
	if inputClient, ok := any(executor).(inputStreamExecutor); ok {
		err = inputClient.ExecuteStreamWithInput(ctx, curlCmd, body, stdout, stderr)
	} else {
		payload, readErr := io.ReadAll(body)
		if readErr != nil {
			return fmt.Errorf("failed to read request body: %w", readErr)
		}
		embedded := "printf '%s' " + shellQuote(base64.StdEncoding.EncodeToString(payload)) + " | base64 -d | " + curlCmd
		if contextClient, ok := any(executor).(contextStreamExecutor); ok {
			err = contextClient.ExecuteStreamWithContext(ctx, embedded, stdout, stderr)
		} else {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			err = executor.ExecuteStream(embedded, stdout, stderr)
		}
	}
	if err != nil {
		return fmt.Errorf("takod stream request %s %s failed: %w", method, endpoint, err)
	}
	return nil
}

func buildStreamRequestOutputCommand(socket string, method string, endpoint string) string {
	args := []string{
		"if test -S " + shellQuote(socket) + " && command -v curl >/dev/null 2>&1; then",
		"curl --fail --silent --show-error --no-buffer",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
		"-H 'Content-Type: application/json'",
		"--data-binary @-",
		shellQuote("http://takod" + endpoint),
		"; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi",
	}
	return strings.Join(args, " ")
}

// ExecEndpoint returns the takod exec endpoint path.
func JobsApplyEndpoint() string {
	return "/v1/jobs/apply"
}

func ExecEndpoint() string {
	return "/v1/exec"
}

func ProxyFileEndpoint(name string) string {
	return "/v1/proxy-file?name=" + url.QueryEscape(name)
}

func CertificatesEndpoint(domain string) string {
	if strings.TrimSpace(domain) == "" {
		return "/v1/certs"
	}
	return "/v1/certs?domain=" + url.QueryEscape(domain)
}

func ScopedCertificatesEndpoint(project, environment, domain string) string {
	return addEndpointScope(CertificatesEndpoint(domain), project, environment)
}

func StateEndpoint(project string, environment string, document string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("document", document)
	return "/v1/state?" + query.Encode()
}

func StateRevisionEndpoint(project string, environment string, document string, revisionID string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("document", document)
	if revisionID != "" {
		query.Set("revisionId", revisionID)
	}
	return "/v1/state?" + query.Encode()
}

func StateNodeEndpoint(project string, environment string, document string, node string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("document", document)
	query.Set("node", node)
	return "/v1/state?" + query.Encode()
}

func LeaseEndpoint(project string, environment string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	return "/v1/lease?" + query.Encode()
}

func ActualStateEndpoint(project string, environment string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	return "/v1/actual?" + query.Encode()
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

func ImageBuildEndpoint(image string, dockerfile ...string) string {
	query := url.Values{}
	query.Set("image", image)
	if len(dockerfile) > 0 && strings.TrimSpace(dockerfile[0]) != "" {
		query.Set("dockerfile", dockerfile[0])
	}
	return "/v1/images/build?" + query.Encode()
}

func ScopedImageBuildEndpoint(project, environment, image string, dockerfile ...string) string {
	return addEndpointScope(ImageBuildEndpoint(image, dockerfile...), project, environment)
}

// ImageBuildEndpointWithAuth marks the build request body as carrying a
// registry-auth JSON preamble line ahead of the tar stream. The flag is the
// only auth-related value in the URL; credentials ride the body.
func ImageBuildEndpointWithAuth(image string, dockerfile ...string) string {
	return ImageBuildEndpoint(image, dockerfile...) + "&auth=preamble"
}

func ScopedImageBuildEndpointWithAuth(project, environment, image string, dockerfile ...string) string {
	return addEndpointScope(ImageBuildEndpointWithAuth(image, dockerfile...), project, environment)
}

// ImageBuildEndpointWithOptions marks the body as carrying a JSON options
// preamble before the build-context archive. Build arg values stay out of URLs.
func ImageBuildEndpointWithOptions(image string, dockerfile string, withAuth bool) string {
	endpoint := ImageBuildEndpoint(image, dockerfile) + "&options=preamble"
	if withAuth {
		endpoint += "&auth=preamble"
	}
	return endpoint
}

func ScopedImageBuildEndpointWithOptions(project, environment, image, dockerfile string, withAuth bool) string {
	return addEndpointScope(ImageBuildEndpointWithOptions(image, dockerfile, withAuth), project, environment)
}

func ImageExistsEndpoint(image string) string {
	query := url.Values{}
	query.Set("image", image)
	return "/v1/images/exists?" + query.Encode()
}

func ImageInspectEndpoint(image string) string {
	query := url.Values{}
	query.Set("image", image)
	return "/v1/images/inspect?" + query.Encode()
}

func ImageExportEndpoint(image string, expectedImageID ...string) string {
	query := url.Values{}
	query.Set("image", image)
	if len(expectedImageID) > 0 && strings.TrimSpace(expectedImageID[0]) != "" {
		query.Set("expectedImageId", strings.TrimSpace(expectedImageID[0]))
	}
	return "/v1/images/export?" + query.Encode()
}

func ImageImportEndpoint(image string, expectedImageID ...string) string {
	query := url.Values{}
	query.Set("image", image)
	if len(expectedImageID) > 0 && strings.TrimSpace(expectedImageID[0]) != "" {
		query.Set("expectedImageId", strings.TrimSpace(expectedImageID[0]))
	}
	return "/v1/images/import?" + query.Encode()
}

func ScopedImageImportEndpoint(project, environment, image string, expectedImageID ...string) string {
	return addEndpointScope(ImageImportEndpoint(image, expectedImageID...), project, environment)
}

func ScopedProxyFileEndpoint(project, environment, name string) string {
	return addEndpointScope(ProxyFileEndpoint(name), project, environment)
}

func addEndpointScope(endpoint, project, environment string) string {
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	return endpoint + separator + query.Encode()
}

func LogsEndpoint(project string, environment string, service string, tail int, follow bool) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("service", service)
	query.Set("tail", fmt.Sprintf("%d", tail))
	if follow {
		query.Set("follow", "true")
	}
	return "/v1/logs?" + query.Encode()
}

func StatsEndpoint(project string, environment string, service string, all bool) string {
	query := url.Values{}
	if project != "" {
		query.Set("project", project)
	}
	if environment != "" {
		query.Set("environment", environment)
	}
	if service != "" {
		query.Set("service", service)
	}
	if all {
		query.Set("all", "true")
	}
	return "/v1/stats?" + query.Encode()
}

func MetricsEndpoint(collect bool) string {
	if !collect {
		return "/v1/metrics"
	}
	query := url.Values{}
	query.Set("collect", "true")
	return "/v1/metrics?" + query.Encode()
}

func AccessLogsEndpoint(tail int, follow bool) string {
	query := url.Values{}
	query.Set("tail", fmt.Sprintf("%d", tail))
	if follow {
		query.Set("follow", "true")
	}
	return "/v1/access-logs?" + query.Encode()
}

func DiscoveryExportsEndpoint(environment string) string {
	if strings.TrimSpace(environment) == "" {
		return "/v1/discovery/exports"
	}
	query := url.Values{}
	query.Set("environment", environment)
	return "/v1/discovery/exports?" + query.Encode()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
