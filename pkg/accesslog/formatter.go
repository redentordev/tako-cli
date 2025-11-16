package accesslog

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AccessLog represents a JSON access log entry (Traefik format)
type AccessLog struct {
	// Old format (for backward compatibility)
	Level   string  `json:"level"`
	Ts      float64 `json:"ts"`
	Logger  string  `json:"logger"`
	Msg     string  `json:"msg"`
	Request struct {
		RemoteIP   string              `json:"remote_ip"`
		RemotePort string              `json:"remote_port"`
		Proto      string              `json:"proto"`
		Method     string              `json:"method"`
		Host       string              `json:"host"`
		URI        string              `json:"uri"`
		Headers    map[string][]string `json:"headers"`
	} `json:"request"`
	UserID      string  `json:"user_id"`
	Duration    float64 `json:"duration"`
	Size        int     `json:"size"`
	Status      int     `json:"status"`
	RespHeaders struct {
		ContentType []string `json:"Content-Type"`
		Server      []string `json:"Server"`
	} `json:"resp_headers"`

	// New Traefik format (Swarm mode)
	ClientAddr            string `json:"ClientAddr"`
	ClientHost            string `json:"ClientHost"`
	DownstreamStatus      int    `json:"DownstreamStatus"`
	RequestMethod         string `json:"RequestMethod"`
	RequestPath           string `json:"RequestPath"`
	RequestProtocol       string `json:"RequestProtocol"`
	RequestHost           string `json:"RequestHost"`
	ServiceName           string `json:"ServiceName"`
	OriginDuration        int64  `json:"OriginDuration"` // nanoseconds
	OriginContentSize     int64  `json:"OriginContentSize"`
	Time                  string `json:"time"`
	DownstreamContentSize int64  `json:"DownstreamContentSize"`
}

// Formatter handles formatting of access logs
type Formatter struct {
	verbose       bool
	serviceFilter string // Filter logs by service name
}

// NewFormatter creates a new log formatter
func NewFormatter(verbose bool) *Formatter {
	return &Formatter{
		verbose:       verbose,
		serviceFilter: "",
	}
}

// SetServiceFilter sets a filter to only show logs for a specific service
func (f *Formatter) SetServiceFilter(serviceName string) {
	f.serviceFilter = serviceName
}

// FormatLine formats a single JSON log line into human-readable output
func (f *Formatter) FormatLine(line string) (string, error) {
	if line == "" {
		return "", nil
	}

	var log AccessLog
	if err := json.Unmarshal([]byte(line), &log); err != nil {
		// Not a JSON line, return as-is
		return line, nil
	}

	// Detect which format we have and extract common fields
	var timestamp string
	var status int
	var method string
	var remoteIP string
	var duration float64
	var size int
	var uri string

	// Apply service filter if set
	if f.serviceFilter != "" {
		// Extract service name from ServiceName field (format: project_env_service@swarm)
		serviceName := log.ServiceName
		if serviceName != "" {
			// Parse format: project_env_service@swarm -> service
			parts := strings.Split(serviceName, "_")
			if len(parts) >= 3 {
				actualService := strings.Split(parts[2], "@")[0]
				if actualService != f.serviceFilter {
					return "", nil // Skip this log entry
				}
			}
		}
	}

	// Check if it's new Traefik format (has RequestMethod field)
	if log.RequestMethod != "" {
		// New Traefik Swarm format
		if log.Time != "" {
			t, err := time.Parse(time.RFC3339, log.Time)
			if err == nil {
				timestamp = t.Format("15:04:05")
			} else {
				timestamp = log.Time[:8] // fallback
			}
		}
		status = log.DownstreamStatus
		method = log.RequestMethod
		remoteIP = log.ClientHost
		duration = float64(log.OriginDuration) / 1000000000.0 // nanoseconds to seconds
		size = int(log.DownstreamContentSize)
		uri = log.RequestPath
	} else if log.Request.Method != "" {
		// Old format
		timestamp = time.Unix(int64(log.Ts), 0).Format("15:04:05")
		status = log.Status
		method = log.Request.Method
		remoteIP = log.Request.RemoteIP
		duration = log.Duration
		size = log.Size
		uri = log.Request.URI
	} else {
		// Skip non-request logs
		return "", nil
	}

	// Format status with color
	statusColor := getStatusColor(status)
	statusStr := fmt.Sprintf("%s%3d%s", statusColor, status, colorReset)

	// Format method with color
	methodColor := getMethodColor(method)
	methodStr := fmt.Sprintf("%s%-6s%s", methodColor, method, colorReset)

	// Format duration
	durationStr := formatDuration(duration)

	// Format size
	sizeStr := formatSize(size)

	// Remote IP (truncate if too long)
	if len(remoteIP) > 15 {
		remoteIP = remoteIP[:15]
	}

	// URI (truncate if too long)
	if len(uri) > 60 && !f.verbose {
		uri = uri[:57] + "..."
	}

	// Build output line
	output := fmt.Sprintf("%s%s%s %s %s %-15s %s %s %s",
		colorGray, timestamp, colorReset,
		statusStr,
		methodStr,
		remoteIP,
		durationStr,
		sizeStr,
		uri,
	)

	// In verbose mode, add more details
	if f.verbose {
		// Add service name for new format
		if log.ServiceName != "" {
			output += fmt.Sprintf("\n  %sService:%s %s", colorGray, colorReset, log.ServiceName)
		}

		// Add request host
		if log.RequestHost != "" {
			output += fmt.Sprintf("\n  %sHost:%s %s", colorGray, colorReset, log.RequestHost)
		}

		// Add user agent for old format
		if userAgent, ok := log.Request.Headers["User-Agent"]; ok && len(userAgent) > 0 {
			output += fmt.Sprintf("\n  %sUA:%s %s", colorGray, colorReset, userAgent[0])
		}

		// Add referer for old format
		if referer, ok := log.Request.Headers["Referer"]; ok && len(referer) > 0 && referer[0] != "" {
			output += fmt.Sprintf("\n  %sReferer:%s %s", colorGray, colorReset, referer[0])
		}
	}

	return output, nil
}

// formatDuration formats duration in milliseconds
func formatDuration(seconds float64) string {
	ms := seconds * 1000

	if ms < 1 {
		return fmt.Sprintf("%s%5.2fms%s", colorGreen, ms, colorReset)
	} else if ms < 100 {
		return fmt.Sprintf("%s%5.2fms%s", colorYellow, ms, colorReset)
	} else if ms < 1000 {
		return fmt.Sprintf("%s%5.2fms%s", colorRed, ms, colorReset)
	} else {
		return fmt.Sprintf("%s%5.2fs%s", colorRed, seconds, colorReset)
	}
}

// formatSize formats bytes into human-readable size
func formatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%4dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%4dK", bytes/1024)
	} else {
		return fmt.Sprintf("%4dM", bytes/(1024*1024))
	}
}

// getStatusColor returns ANSI color code for HTTP status
func getStatusColor(status int) string {
	switch {
	case status >= 200 && status < 300:
		return colorGreen
	case status >= 300 && status < 400:
		return colorCyan
	case status >= 400 && status < 500:
		return colorYellow
	case status >= 500:
		return colorRed
	default:
		return colorReset
	}
}

// getMethodColor returns ANSI color code for HTTP method
func getMethodColor(method string) string {
	switch strings.ToUpper(method) {
	case "GET":
		return colorCyan
	case "POST":
		return colorGreen
	case "PUT":
		return colorYellow
	case "DELETE":
		return colorRed
	case "PATCH":
		return colorMagenta
	default:
		return colorReset
	}
}

// ANSI color codes
const (
	colorReset   = "\033[0m"
	colorRed     = "\033[31m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorCyan    = "\033[36m"
	colorMagenta = "\033[35m"
	colorGray    = "\033[90m"
)

// FormatHeader returns a formatted header for the log output
func (f *Formatter) FormatHeader() string {
	return fmt.Sprintf("%s%-8s %-4s %-6s %-15s %-8s %-6s %s%s",
		colorGray,
		"TIME",
		"CODE",
		"METHOD",
		"IP",
		"DURATION",
		"SIZE",
		"PATH",
		colorReset,
	)
}
