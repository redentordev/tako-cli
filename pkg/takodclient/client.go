package takodclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultSocket = "/run/tako/takod.sock"

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

func RequestJSON(client RequestExecutor, socket string, method string, endpoint string, value any) (string, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return "", fmt.Errorf("takod endpoint must start with /")
	}
	if method == "" {
		method = "GET"
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
	ctx, cancel := context.WithTimeout(context.Background(), JSONRequestTimeout)
	defer cancel()

	var output string
	var err error
	if hasBody {
		output, err = client.ExecuteWithInput(ctx, curlCmd, body)
	} else {
		output, err = client.ExecuteWithContext(ctx, curlCmd)
	}
	bodyOutput, status, hasStatus := splitHTTPStatus(output)
	if err != nil {
		return bodyOutput, fmt.Errorf("takod request %s %s failed: %w, output: %s", method, endpoint, err, bodyOutput)
	}
	bodyOutput = sanitizeJSONOutput(bodyOutput)
	if hasStatus && status >= 400 {
		return bodyOutput, fmt.Errorf("takod request %s %s returned HTTP %d: %s", method, endpoint, status, strings.TrimSpace(bodyOutput))
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

func StreamRequest(client RequestExecutor, socket string, method string, endpoint string, reader io.Reader) (string, error) {
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
		"curl --silent --show-error",
		"--write-out '\\n__TAKO_HTTP_STATUS__:%{http_code}'",
		"--unix-socket " + shellQuote(socket),
		"-X " + shellQuote(method),
		"--data-binary @-",
		shellQuote("http://takod" + endpoint),
		"; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi",
	}
	curlCmd := strings.Join(args, " ")
	ctx, cancel := context.WithTimeout(context.Background(), StreamRequestTimeout)
	defer cancel()

	output, err := client.ExecuteWithInput(ctx, curlCmd, reader)
	bodyOutput, status, hasStatus := splitHTTPStatus(output)
	if err != nil {
		return bodyOutput, fmt.Errorf("takod stream request %s %s failed: %w, output: %s", method, endpoint, err, bodyOutput)
	}
	bodyOutput = sanitizeJSONOutput(bodyOutput)
	if hasStatus && status >= 400 {
		return bodyOutput, fmt.Errorf("takod stream request %s %s returned HTTP %d: %s", method, endpoint, status, strings.TrimSpace(bodyOutput))
	}
	return bodyOutput, nil
}

func StreamOutput(client StreamExecutor, socket string, endpoint string, stdout io.Writer, stderr io.Writer) error {
	if socket == "" {
		socket = DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return fmt.Errorf("takod endpoint must start with /")
	}

	args := []string{
		"if test -S " + shellQuote(socket) + " && command -v curl >/dev/null 2>&1; then",
		"curl --fail --silent --show-error --no-buffer",
		"--unix-socket " + shellQuote(socket),
		shellQuote("http://takod" + endpoint),
		"; else echo 'takod socket or curl is unavailable' >&2; exit 42; fi",
	}
	curlCmd := strings.Join(args, " ")
	if err := client.ExecuteStream(curlCmd, stdout, stderr); err != nil {
		return fmt.Errorf("takod stream request %s failed: %w", endpoint, err)
	}
	return nil
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

func InspectEndpoint(project string, environment string, service string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	if service != "" {
		query.Set("service", service)
	}
	return "/v1/inspect?" + query.Encode()
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

type ImageBuildEndpointOptions struct {
	Platform   string
	Dockerfile string
	CacheFrom  []string
	CacheTo    []string
	Builder    string
	Buildx     bool
}

func ImageBuildEndpoint(image string, platform ...string) string {
	opts := ImageBuildEndpointOptions{}
	if len(platform) > 0 {
		opts.Platform = platform[0]
	}
	return ImageBuildEndpointWithOptions(image, opts)
}

func ImageBuildEndpointWithOptions(image string, opts ImageBuildEndpointOptions) string {
	query := url.Values{}
	query.Set("image", image)
	if opts.Platform != "" {
		query.Set("platform", opts.Platform)
	}
	if opts.Dockerfile != "" {
		query.Set("dockerfile", opts.Dockerfile)
	}
	for _, cacheFrom := range opts.CacheFrom {
		if cacheFrom != "" {
			query.Add("cacheFrom", cacheFrom)
		}
	}
	for _, cacheTo := range opts.CacheTo {
		if cacheTo != "" {
			query.Add("cacheTo", cacheTo)
		}
	}
	if opts.Builder != "" {
		query.Set("builder", opts.Builder)
	}
	if opts.Buildx {
		query.Set("buildx", "true")
	}
	return "/v1/images/build?" + query.Encode()
}

func ImagesEndpoint(project string, environment string) string {
	query := url.Values{}
	query.Set("project", project)
	if environment != "" {
		query.Set("environment", environment)
	}
	return "/v1/images?" + query.Encode()
}

func VolumesEndpoint(project string, environment string) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	return "/v1/volumes?" + query.Encode()
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

func NodeLogsEndpoint(unit string, tail int, follow bool) string {
	query := url.Values{}
	if unit != "" {
		query.Set("unit", unit)
	}
	query.Set("tail", fmt.Sprintf("%d", tail))
	if follow {
		query.Set("follow", "true")
	}
	return "/v1/node/logs?" + query.Encode()
}

func NodeInfoEndpoint() string {
	return "/v1/node/info"
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

func PrometheusMetricsEndpoint(project string, environment string, collect bool) string {
	query := url.Values{}
	query.Set("format", "prometheus")
	if project != "" {
		query.Set("project", project)
	}
	if environment != "" {
		query.Set("environment", environment)
	}
	if collect {
		query.Set("collect", "true")
	}
	return "/v1/metrics?" + query.Encode()
}

func ProxyTargetEndpoint(project string, environment string, service string, port int) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("service", service)
	query.Set("port", fmt.Sprintf("%d", port))
	return "/v1/proxy-target?" + query.Encode()
}

func DiscoveryEndpoint(project string, environment string, service string, port int, roundRobin bool) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	if service != "" {
		query.Set("service", service)
	}
	if port > 0 {
		query.Set("port", fmt.Sprintf("%d", port))
	}
	if roundRobin {
		query.Set("roundRobin", "true")
	}
	return "/v1/discovery?" + query.Encode()
}

func ExecTargetEndpoint(project string, environment string, service string, slot int) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	query.Set("service", service)
	if slot > 0 {
		query.Set("slot", fmt.Sprintf("%d", slot))
	}
	return "/v1/exec-target?" + query.Encode()
}

func MeshRTTEndpoint(target string, count int) string {
	query := url.Values{}
	query.Set("target", target)
	if count > 0 {
		query.Set("count", fmt.Sprintf("%d", count))
	}
	return "/v1/mesh/rtt?" + query.Encode()
}

func AccessLogsEndpoint(tail int, follow bool) string {
	query := url.Values{}
	query.Set("tail", fmt.Sprintf("%d", tail))
	if follow {
		query.Set("follow", "true")
	}
	return "/v1/access-logs?" + query.Encode()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
