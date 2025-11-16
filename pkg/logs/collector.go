package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

// LogEntry represents a unified log entry (container or access log)
type LogEntry struct {
	Timestamp   time.Time         `json:"timestamp"`
	Source      string            `json:"source"`     // "container" or "access"
	Project     string            `json:"project"`    // Project name
	Service     string            `json:"service"`    // Service name
	Server      string            `json:"server"`     // Server name
	Message     string            `json:"message"`    // Raw log message (for container logs)
	Level       string            `json:"level"`      // Log level (INFO, ERROR, etc.)
	AccessLog   *AccessLogEntry   `json:"access_log"` // Access log details (if source=access)
	ContainerID string            `json:"container_id"`
	Tags        map[string]string `json:"tags"` // Additional metadata
}

// AccessLogEntry represents Traefik access log details
type AccessLogEntry struct {
	Method       string  `json:"method"`        // HTTP method (GET, POST, etc.)
	Path         string  `json:"path"`          // Request path
	StatusCode   int     `json:"status_code"`   // HTTP status code
	Duration     float64 `json:"duration_ms"`   // Response time in milliseconds
	BytesOut     int64   `json:"bytes_out"`     // Response size
	ClientIP     string  `json:"client_ip"`     // Client IP address
	UserAgent    string  `json:"user_agent"`    // User agent string
	Referrer     string  `json:"referrer"`      // HTTP referrer
	Host         string  `json:"host"`          // Request host/domain
	Protocol     string  `json:"protocol"`      // HTTP/HTTPS
	RouterName   string  `json:"router_name"`   // Traefik router name
	ServiceName  string  `json:"service_name"`  // Traefik service name
	DownstreamIP string  `json:"downstream_ip"` // Original client IP (behind proxy)
}

// Collector handles log collection from services
type Collector struct {
	client *ssh.Client
	server string
}

// NewCollector creates a new log collector
func NewCollector(client *ssh.Client, server string) *Collector {
	return &Collector{
		client: client,
		server: server,
	}
}

// GetContainerLogs fetches recent container logs for a service
func (c *Collector) GetContainerLogs(project, service string, lines int) ([]*LogEntry, error) {
	// Find containers for this service
	findCmd := fmt.Sprintf("docker ps --filter name=%s_%s --format '{{.Names}}'", project, service)
	output, err := c.client.Execute(findCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to find containers: %w", err)
	}

	containers := strings.Split(strings.TrimSpace(output), "\n")
	if len(containers) == 0 || containers[0] == "" {
		return nil, fmt.Errorf("no containers found for %s/%s", project, service)
	}

	var allLogs []*LogEntry

	for _, container := range containers {
		if container == "" {
			continue
		}

		// Get logs with timestamps
		logsCmd := fmt.Sprintf("docker logs --tail %d --timestamps %s 2>&1", lines, container)
		logsOutput, err := c.client.Execute(logsCmd)
		if err != nil {
			continue // Skip this container on error
		}

		// Parse logs
		scanner := bufio.NewScanner(strings.NewReader(logsOutput))
		for scanner.Scan() {
			line := scanner.Text()
			entry := c.parseContainerLog(line, project, service, container)
			if entry != nil {
				allLogs = append(allLogs, entry)
			}
		}
	}

	// Sort by timestamp (newest first)
	sort.Slice(allLogs, func(i, j int) bool {
		return allLogs[i].Timestamp.After(allLogs[j].Timestamp)
	})

	return allLogs, nil
}

// GetAccessLogs fetches recent access logs for a project
func (c *Collector) GetAccessLogs(project string, lines int) ([]*LogEntry, error) {
	// Read Traefik access logs
	logsCmd := fmt.Sprintf("sudo tail -n %d /var/log/traefik/access.log 2>/dev/null || echo ''", lines)
	output, err := c.client.Execute(logsCmd)
	if err != nil || output == "" {
		return nil, fmt.Errorf("failed to read access logs: %w", err)
	}

	var logs []*LogEntry
	scanner := bufio.NewScanner(strings.NewReader(output))

	for scanner.Scan() {
		line := scanner.Text()
		entry := c.parseAccessLog(line, project)
		if entry != nil {
			logs = append(logs, entry)
		}
	}

	// Sort by timestamp (newest first)
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})

	return logs, nil
}

// StreamContainerLogs streams container logs in real-time
func (c *Collector) StreamContainerLogs(project, service string, callback func(*LogEntry)) error {
	// Find containers
	findCmd := fmt.Sprintf("docker ps --filter name=%s_%s --format '{{.Names}}'", project, service)
	output, err := c.client.Execute(findCmd)
	if err != nil {
		return fmt.Errorf("failed to find containers: %w", err)
	}

	containers := strings.Split(strings.TrimSpace(output), "\n")
	if len(containers) == 0 || containers[0] == "" {
		return fmt.Errorf("no containers found")
	}

	// Stream logs from first container (can extend to all)
	container := containers[0]
	streamCmd := fmt.Sprintf("docker logs -f --timestamps %s 2>&1", container)

	session, err := c.client.StartStream(streamCmd)
	if err != nil {
		return fmt.Errorf("failed to start log stream: %w", err)
	}
	defer session.Close()

	// Read logs line by line
	scanner := bufio.NewScanner(session.Stdout)
	for scanner.Scan() {
		line := scanner.Text()
		entry := c.parseContainerLog(line, project, service, container)
		if entry != nil {
			callback(entry)
		}
	}

	return scanner.Err()
}

// parseContainerLog parses a Docker container log line
func (c *Collector) parseContainerLog(line, project, service, container string) *LogEntry {
	if line == "" {
		return nil
	}

	// Docker logs format: 2025-01-15T10:30:45.123456789Z actual log message
	parts := strings.SplitN(line, " ", 2)
	if len(parts) < 2 {
		// No timestamp, use current time
		return &LogEntry{
			Timestamp:   time.Now(),
			Source:      "container",
			Project:     project,
			Service:     service,
			Server:      c.server,
			Message:     line,
			Level:       c.detectLogLevel(line),
			ContainerID: container,
		}
	}

	// Parse timestamp
	timestamp, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		timestamp = time.Now()
	}

	message := parts[1]

	return &LogEntry{
		Timestamp:   timestamp,
		Source:      "container",
		Project:     project,
		Service:     service,
		Server:      c.server,
		Message:     message,
		Level:       c.detectLogLevel(message),
		ContainerID: container,
	}
}

// parseAccessLog parses a Traefik access log line (JSON format)
func (c *Collector) parseAccessLog(line, project string) *LogEntry {
	if line == "" {
		return nil
	}

	// Try to parse as JSON first (if we configure Traefik for JSON logs)
	var jsonLog map[string]interface{}
	if err := json.Unmarshal([]byte(line), &jsonLog); err == nil {
		return c.parseJSONAccessLog(jsonLog, project)
	}

	// Fall back to Common Log Format parsing
	return c.parseCommonLogFormat(line, project)
}

// parseJSONAccessLog parses JSON-formatted Traefik access log
func (c *Collector) parseJSONAccessLog(data map[string]interface{}, project string) *LogEntry {
	// Extract timestamp
	timestamp := time.Now()
	if ts, ok := data["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			timestamp = parsed
		}
	}

	// Extract access log details
	accessLog := &AccessLogEntry{
		Method:      getStringField(data, "RequestMethod"),
		Path:        getStringField(data, "RequestPath"),
		StatusCode:  getIntField(data, "DownstreamStatus"),
		Duration:    getFloatField(data, "Duration") / 1000000, // Convert ns to ms
		BytesOut:    int64(getFloatField(data, "DownstreamContentSize")),
		ClientIP:    getStringField(data, "ClientAddr"),
		UserAgent:   getStringField(data, "RequestUserAgent"),
		Referrer:    getStringField(data, "RequestReferer"),
		Host:        getStringField(data, "RequestHost"),
		Protocol:    getStringField(data, "RequestProtocol"),
		RouterName:  getStringField(data, "RouterName"),
		ServiceName: getStringField(data, "ServiceName"),
	}

	// Extract client IP (handle X-Forwarded-For)
	if xff, ok := data["request_X-Forwarded-For"].(string); ok {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			accessLog.DownstreamIP = strings.TrimSpace(parts[0])
		}
	}

	// Determine service from router name
	service := extractServiceFromRouter(accessLog.RouterName, project)

	return &LogEntry{
		Timestamp: timestamp,
		Source:    "access",
		Project:   project,
		Service:   service,
		Server:    c.server,
		Level:     c.getStatusLevel(accessLog.StatusCode),
		AccessLog: accessLog,
	}
}

// parseCommonLogFormat parses Common Log Format (CLF) access logs
func (c *Collector) parseCommonLogFormat(line, project string) *LogEntry {
	// CLF: 127.0.0.1 - - [10/Jan/2025:10:30:45 +0000] "GET /api/users HTTP/1.1" 200 1234
	// This is a simplified parser - extend as needed

	parts := strings.Fields(line)
	if len(parts) < 10 {
		return nil
	}

	// Extract IP
	clientIP := parts[0]

	// Extract timestamp
	timestampStr := strings.Trim(parts[3]+parts[4], "[]")
	timestamp, _ := time.Parse("02/Jan/2006:15:04:05 -0700", timestampStr)

	// Extract request
	request := strings.Trim(parts[5]+parts[6]+parts[7], "\"")
	requestParts := strings.Fields(request)

	method := ""
	path := ""
	protocol := ""
	if len(requestParts) >= 3 {
		method = requestParts[0]
		path = requestParts[1]
		protocol = requestParts[2]
	}

	// Extract status code
	statusCode := 0
	fmt.Sscanf(parts[8], "%d", &statusCode)

	// Extract bytes
	bytesOut := int64(0)
	fmt.Sscanf(parts[9], "%d", &bytesOut)

	return &LogEntry{
		Timestamp: timestamp,
		Source:    "access",
		Project:   project,
		Server:    c.server,
		Level:     c.getStatusLevel(statusCode),
		AccessLog: &AccessLogEntry{
			Method:     method,
			Path:       path,
			StatusCode: statusCode,
			BytesOut:   bytesOut,
			ClientIP:   clientIP,
			Protocol:   protocol,
		},
	}
}

// Helper functions

func (c *Collector) detectLogLevel(message string) string {
	messageLower := strings.ToLower(message)

	if strings.Contains(messageLower, "error") || strings.Contains(messageLower, "fatal") {
		return "ERROR"
	}
	if strings.Contains(messageLower, "warn") {
		return "WARN"
	}
	if strings.Contains(messageLower, "debug") {
		return "DEBUG"
	}
	return "INFO"
}

func (c *Collector) getStatusLevel(statusCode int) string {
	if statusCode >= 500 {
		return "ERROR"
	}
	if statusCode >= 400 {
		return "WARN"
	}
	if statusCode >= 300 {
		return "INFO"
	}
	return "INFO"
}

func extractServiceFromRouter(routerName, project string) string {
	// Router name format: {project}_{env}_{service}-0
	// Extract service name
	parts := strings.Split(routerName, "_")
	if len(parts) >= 3 {
		servicePart := parts[len(parts)-1]
		return strings.Split(servicePart, "-")[0]
	}
	return ""
}

func getStringField(data map[string]interface{}, key string) string {
	if val, ok := data[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

func getIntField(data map[string]interface{}, key string) int {
	if val, ok := data[key]; ok {
		if num, ok := val.(float64); ok {
			return int(num)
		}
	}
	return 0
}

func getFloatField(data map[string]interface{}, key string) float64 {
	if val, ok := data[key]; ok {
		if num, ok := val.(float64); ok {
			return num
		}
	}
	return 0.0
}
