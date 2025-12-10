package notification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// EventType represents the type of deployment event
type EventType string

const (
	EventDeployStarted   EventType = "deploy_started"
	EventDeploySucceeded EventType = "deploy_succeeded"
	EventDeployFailed    EventType = "deploy_failed"
	EventRollbackStarted EventType = "rollback_started"
	EventRollbackDone    EventType = "rollback_done"
	EventServiceDown     EventType = "service_down"
	EventServiceUp       EventType = "service_up"
	EventServiceRestarted EventType = "service_restarted"
	EventDriftDetected   EventType = "drift_detected"
	EventBackupCompleted EventType = "backup_completed"
	EventBackupFailed    EventType = "backup_failed"
	// Resource alerts
	EventHighCPU         EventType = "high_cpu"
	EventHighMemory      EventType = "high_memory"
	EventHighDisk        EventType = "high_disk"
	EventResourceNormal  EventType = "resource_normal"
	// Health alerts
	EventHealthCheckFailed  EventType = "health_check_failed"
	EventHealthCheckRecovered EventType = "health_check_recovered"
	EventContainerOOM       EventType = "container_oom"
	EventContainerCrashLoop EventType = "container_crash_loop"
	// SSL alerts
	EventSSLExpiringSoon  EventType = "ssl_expiring_soon"
	EventSSLExpired       EventType = "ssl_expired"
	EventSSLRenewed       EventType = "ssl_renewed"
	// Scaling events
	EventScaleUp          EventType = "scale_up"
	EventScaleDown        EventType = "scale_down"
)

// Event represents a notification event
type Event struct {
	Type        EventType         `json:"type"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Service     string            `json:"service,omitempty"`
	Message     string            `json:"message"`
	Details     map[string]string `json:"details,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
	Duration    time.Duration     `json:"duration,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// NotifierConfig holds configuration for notifications
type NotifierConfig struct {
	SlackWebhook   string `yaml:"slackWebhook,omitempty"`
	DiscordWebhook string `yaml:"discordWebhook,omitempty"`
	Webhook        string `yaml:"webhook,omitempty"` // Generic webhook
	Email          string `yaml:"email,omitempty"`   // Future: email notifications
}

// Notifier handles sending notifications
type Notifier struct {
	config  NotifierConfig
	client  *http.Client
	verbose bool
}

// NewNotifier creates a new notifier
func NewNotifier(config NotifierConfig, verbose bool) *Notifier {
	return &Notifier{
		config: config,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		verbose: verbose,
	}
}

// Notify sends a notification to all configured channels
func (n *Notifier) Notify(event Event) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	var errors []string

	// Send to Slack
	if n.config.SlackWebhook != "" {
		if err := n.sendSlack(event); err != nil {
			errors = append(errors, fmt.Sprintf("slack: %v", err))
		}
	}

	// Send to Discord
	if n.config.DiscordWebhook != "" {
		if err := n.sendDiscord(event); err != nil {
			errors = append(errors, fmt.Sprintf("discord: %v", err))
		}
	}

	// Send to generic webhook
	if n.config.Webhook != "" {
		if err := n.sendWebhook(event); err != nil {
			errors = append(errors, fmt.Sprintf("webhook: %v", err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("notification errors: %s", strings.Join(errors, "; "))
	}

	return nil
}

// sendSlack sends a notification to Slack
func (n *Notifier) sendSlack(event Event) error {
	color := n.getEventColor(event.Type)
	emoji := n.getEventEmoji(event.Type)

	// Build Slack message with blocks
	payload := map[string]interface{}{
		"attachments": []map[string]interface{}{
			{
				"color": color,
				"blocks": []map[string]interface{}{
					{
						"type": "header",
						"text": map[string]string{
							"type":  "plain_text",
							"text":  fmt.Sprintf("%s %s", emoji, n.getEventTitle(event.Type)),
							"emoji": "true",
						},
					},
					{
						"type": "section",
						"fields": []map[string]string{
							{"type": "mrkdwn", "text": fmt.Sprintf("*Project:*\n%s", event.Project)},
							{"type": "mrkdwn", "text": fmt.Sprintf("*Environment:*\n%s", event.Environment)},
						},
					},
					{
						"type": "section",
						"text": map[string]string{
							"type": "mrkdwn",
							"text": event.Message,
						},
					},
				},
			},
		},
	}

	// Add service field if present
	if event.Service != "" {
		payload["attachments"].([]map[string]interface{})[0]["blocks"] = append(
			payload["attachments"].([]map[string]interface{})[0]["blocks"].([]map[string]interface{}),
			map[string]interface{}{
				"type": "section",
				"fields": []map[string]string{
					{"type": "mrkdwn", "text": fmt.Sprintf("*Service:*\n%s", event.Service)},
				},
			},
		)
	}

	// Add error if present
	if event.Error != "" {
		payload["attachments"].([]map[string]interface{})[0]["blocks"] = append(
			payload["attachments"].([]map[string]interface{})[0]["blocks"].([]map[string]interface{}),
			map[string]interface{}{
				"type": "section",
				"text": map[string]string{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*Error:*\n```%s```", event.Error),
				},
			},
		)
	}

	// Add timestamp
	payload["attachments"].([]map[string]interface{})[0]["blocks"] = append(
		payload["attachments"].([]map[string]interface{})[0]["blocks"].([]map[string]interface{}),
		map[string]interface{}{
			"type": "context",
			"elements": []map[string]string{
				{"type": "mrkdwn", "text": fmt.Sprintf("<!date^%d^{date_short_pretty} at {time}|%s>",
					event.Timestamp.Unix(), event.Timestamp.Format(time.RFC3339))},
			},
		},
	)

	return n.postJSON(n.config.SlackWebhook, payload)
}

// sendDiscord sends a notification to Discord
func (n *Notifier) sendDiscord(event Event) error {
	color := n.getEventColorInt(event.Type)

	// Build Discord embed
	embed := map[string]interface{}{
		"title":       fmt.Sprintf("%s %s", n.getEventEmoji(event.Type), n.getEventTitle(event.Type)),
		"description": event.Message,
		"color":       color,
		"fields": []map[string]interface{}{
			{"name": "Project", "value": event.Project, "inline": true},
			{"name": "Environment", "value": event.Environment, "inline": true},
		},
		"timestamp": event.Timestamp.Format(time.RFC3339),
	}

	if event.Service != "" {
		embed["fields"] = append(embed["fields"].([]map[string]interface{}),
			map[string]interface{}{"name": "Service", "value": event.Service, "inline": true})
	}

	if event.Error != "" {
		embed["fields"] = append(embed["fields"].([]map[string]interface{}),
			map[string]interface{}{"name": "Error", "value": fmt.Sprintf("```%s```", event.Error), "inline": false})
	}

	if event.Duration > 0 {
		embed["fields"] = append(embed["fields"].([]map[string]interface{}),
			map[string]interface{}{"name": "Duration", "value": event.Duration.Round(time.Second).String(), "inline": true})
	}

	payload := map[string]interface{}{
		"embeds": []map[string]interface{}{embed},
	}

	return n.postJSON(n.config.DiscordWebhook, payload)
}

// sendWebhook sends a notification to a generic webhook
func (n *Notifier) sendWebhook(event Event) error {
	return n.postJSON(n.config.Webhook, event)
}

// postJSON sends a JSON payload to a URL
func (n *Notifier) postJSON(url string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	if n.verbose {
		fmt.Printf("  → Notification sent to %s\n", url)
	}

	return nil
}

// getEventColor returns a hex color for the event type (Slack)
func (n *Notifier) getEventColor(eventType EventType) string {
	switch eventType {
	case EventDeploySucceeded, EventServiceUp, EventBackupCompleted, EventRollbackDone,
		EventHealthCheckRecovered, EventResourceNormal, EventSSLRenewed:
		return "#36a64f" // Green
	case EventDeployFailed, EventServiceDown, EventBackupFailed,
		EventContainerOOM, EventContainerCrashLoop, EventSSLExpired:
		return "#dc3545" // Red
	case EventDeployStarted, EventRollbackStarted, EventScaleUp, EventScaleDown, EventServiceRestarted:
		return "#007bff" // Blue
	case EventDriftDetected, EventHighCPU, EventHighMemory, EventHighDisk,
		EventHealthCheckFailed, EventSSLExpiringSoon:
		return "#ffc107" // Yellow/Warning
	default:
		return "#6c757d" // Gray
	}
}

// getEventColorInt returns an integer color for Discord
func (n *Notifier) getEventColorInt(eventType EventType) int {
	switch eventType {
	case EventDeploySucceeded, EventServiceUp, EventBackupCompleted, EventRollbackDone,
		EventHealthCheckRecovered, EventResourceNormal, EventSSLRenewed:
		return 0x36a64f // Green
	case EventDeployFailed, EventServiceDown, EventBackupFailed,
		EventContainerOOM, EventContainerCrashLoop, EventSSLExpired:
		return 0xdc3545 // Red
	case EventDeployStarted, EventRollbackStarted, EventScaleUp, EventScaleDown, EventServiceRestarted:
		return 0x007bff // Blue
	case EventDriftDetected, EventHighCPU, EventHighMemory, EventHighDisk,
		EventHealthCheckFailed, EventSSLExpiringSoon:
		return 0xffc107 // Yellow
	default:
		return 0x6c757d // Gray
	}
}

// getEventEmoji returns an emoji for the event type
func (n *Notifier) getEventEmoji(eventType EventType) string {
	switch eventType {
	case EventDeployStarted:
		return "🚀"
	case EventDeploySucceeded:
		return "✅"
	case EventDeployFailed:
		return "❌"
	case EventRollbackStarted:
		return "⏪"
	case EventRollbackDone:
		return "↩️"
	case EventServiceDown:
		return "🔴"
	case EventServiceUp:
		return "🟢"
	case EventServiceRestarted:
		return "🔄"
	case EventDriftDetected:
		return "⚠️"
	case EventBackupCompleted:
		return "💾"
	case EventBackupFailed:
		return "⚠️"
	case EventHighCPU:
		return "🔥"
	case EventHighMemory:
		return "💾"
	case EventHighDisk:
		return "💿"
	case EventResourceNormal:
		return "✅"
	case EventHealthCheckFailed:
		return "💔"
	case EventHealthCheckRecovered:
		return "💚"
	case EventContainerOOM:
		return "💥"
	case EventContainerCrashLoop:
		return "🔁"
	case EventSSLExpiringSoon:
		return "🔐"
	case EventSSLExpired:
		return "🔓"
	case EventSSLRenewed:
		return "🔒"
	case EventScaleUp:
		return "📈"
	case EventScaleDown:
		return "📉"
	default:
		return "📢"
	}
}

// getEventTitle returns a human-readable title for the event type
func (n *Notifier) getEventTitle(eventType EventType) string {
	switch eventType {
	case EventDeployStarted:
		return "Deployment Started"
	case EventDeploySucceeded:
		return "Deployment Succeeded"
	case EventDeployFailed:
		return "Deployment Failed"
	case EventRollbackStarted:
		return "Rollback Started"
	case EventRollbackDone:
		return "Rollback Completed"
	case EventServiceDown:
		return "Service Down"
	case EventServiceUp:
		return "Service Recovered"
	case EventServiceRestarted:
		return "Service Restarted"
	case EventDriftDetected:
		return "Configuration Drift Detected"
	case EventBackupCompleted:
		return "Backup Completed"
	case EventBackupFailed:
		return "Backup Failed"
	case EventHighCPU:
		return "High CPU Usage Alert"
	case EventHighMemory:
		return "High Memory Usage Alert"
	case EventHighDisk:
		return "High Disk Usage Alert"
	case EventResourceNormal:
		return "Resource Usage Normal"
	case EventHealthCheckFailed:
		return "Health Check Failed"
	case EventHealthCheckRecovered:
		return "Health Check Recovered"
	case EventContainerOOM:
		return "Container Out of Memory"
	case EventContainerCrashLoop:
		return "Container Crash Loop Detected"
	case EventSSLExpiringSoon:
		return "SSL Certificate Expiring Soon"
	case EventSSLExpired:
		return "SSL Certificate Expired"
	case EventSSLRenewed:
		return "SSL Certificate Renewed"
	case EventScaleUp:
		return "Service Scaled Up"
	case EventScaleDown:
		return "Service Scaled Down"
	default:
		return "Tako Notification"
	}
}

// Helper functions to create common events

// DeployStartedEvent creates a deploy started event
func DeployStartedEvent(project, env, service string) Event {
	return Event{
		Type:        EventDeployStarted,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Starting deployment of `%s` to `%s`", service, env),
		Timestamp:   time.Now(),
	}
}

// DeploySucceededEvent creates a deploy succeeded event
func DeploySucceededEvent(project, env, service string, duration time.Duration) Event {
	return Event{
		Type:        EventDeploySucceeded,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Successfully deployed `%s` to `%s`", service, env),
		Duration:    duration,
		Timestamp:   time.Now(),
	}
}

// DeployFailedEvent creates a deploy failed event
func DeployFailedEvent(project, env, service string, err error) Event {
	return Event{
		Type:        EventDeployFailed,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Failed to deploy `%s` to `%s`", service, env),
		Error:       err.Error(),
		Timestamp:   time.Now(),
	}
}

// DriftDetectedEvent creates a drift detected event
func DriftDetectedEvent(project, env, service, description string) Event {
	return Event{
		Type:        EventDriftDetected,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     description,
		Timestamp:   time.Now(),
	}
}

// HighCPUEvent creates a high CPU usage alert event
func HighCPUEvent(project, env, service string, cpuPercent float64, threshold float64) Event {
	return Event{
		Type:        EventHighCPU,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("CPU usage at %.1f%% (threshold: %.1f%%)", cpuPercent, threshold),
		Details: map[string]string{
			"cpu_percent": fmt.Sprintf("%.1f", cpuPercent),
			"threshold":   fmt.Sprintf("%.1f", threshold),
		},
		Timestamp: time.Now(),
	}
}

// HighMemoryEvent creates a high memory usage alert event
func HighMemoryEvent(project, env, service string, memPercent float64, threshold float64, usedMB, totalMB int64) Event {
	return Event{
		Type:        EventHighMemory,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Memory usage at %.1f%% (%dMB / %dMB, threshold: %.1f%%)", memPercent, usedMB, totalMB, threshold),
		Details: map[string]string{
			"mem_percent": fmt.Sprintf("%.1f", memPercent),
			"used_mb":     fmt.Sprintf("%d", usedMB),
			"total_mb":    fmt.Sprintf("%d", totalMB),
			"threshold":   fmt.Sprintf("%.1f", threshold),
		},
		Timestamp: time.Now(),
	}
}

// HighDiskEvent creates a high disk usage alert event
func HighDiskEvent(project, env string, diskPercent float64, threshold float64, usedGB, totalGB int64) Event {
	return Event{
		Type:        EventHighDisk,
		Project:     project,
		Environment: env,
		Message:     fmt.Sprintf("Disk usage at %.1f%% (%dGB / %dGB, threshold: %.1f%%)", diskPercent, usedGB, totalGB, threshold),
		Details: map[string]string{
			"disk_percent": fmt.Sprintf("%.1f", diskPercent),
			"used_gb":      fmt.Sprintf("%d", usedGB),
			"total_gb":     fmt.Sprintf("%d", totalGB),
			"threshold":    fmt.Sprintf("%.1f", threshold),
		},
		Timestamp: time.Now(),
	}
}

// HealthCheckFailedEvent creates a health check failure event
func HealthCheckFailedEvent(project, env, service string, endpoint string, statusCode int, err error) Event {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	} else if statusCode > 0 {
		errMsg = fmt.Sprintf("HTTP %d", statusCode)
	}
	return Event{
		Type:        EventHealthCheckFailed,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Health check failed for `%s`: %s", endpoint, errMsg),
		Error:       errMsg,
		Details: map[string]string{
			"endpoint":    endpoint,
			"status_code": fmt.Sprintf("%d", statusCode),
		},
		Timestamp: time.Now(),
	}
}

// HealthCheckRecoveredEvent creates a health check recovery event
func HealthCheckRecoveredEvent(project, env, service string, endpoint string, downtime time.Duration) Event {
	return Event{
		Type:        EventHealthCheckRecovered,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Health check recovered for `%s` after %s", endpoint, downtime.Round(time.Second)),
		Duration:    downtime,
		Details: map[string]string{
			"endpoint": endpoint,
			"downtime": downtime.Round(time.Second).String(),
		},
		Timestamp: time.Now(),
	}
}

// ContainerOOMEvent creates an out of memory event
func ContainerOOMEvent(project, env, service string, containerID string) Event {
	return Event{
		Type:        EventContainerOOM,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Container `%s` was killed due to out of memory (OOM)", service),
		Error:       "Container killed by OOM killer",
		Details: map[string]string{
			"container_id": containerID,
		},
		Timestamp: time.Now(),
	}
}

// ContainerCrashLoopEvent creates a crash loop event
func ContainerCrashLoopEvent(project, env, service string, restartCount int, lastError string) Event {
	return Event{
		Type:        EventContainerCrashLoop,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Container `%s` is in crash loop (%d restarts)", service, restartCount),
		Error:       lastError,
		Details: map[string]string{
			"restart_count": fmt.Sprintf("%d", restartCount),
			"last_error":    lastError,
		},
		Timestamp: time.Now(),
	}
}

// SSLExpiringSoonEvent creates an SSL expiring soon event
func SSLExpiringSoonEvent(project, env string, domain string, expiresAt time.Time, daysUntilExpiry int) Event {
	return Event{
		Type:        EventSSLExpiringSoon,
		Project:     project,
		Environment: env,
		Message:     fmt.Sprintf("SSL certificate for `%s` expires in %d days (%s)", domain, daysUntilExpiry, expiresAt.Format("2006-01-02")),
		Details: map[string]string{
			"domain":      domain,
			"expires_at":  expiresAt.Format(time.RFC3339),
			"days_left":   fmt.Sprintf("%d", daysUntilExpiry),
		},
		Timestamp: time.Now(),
	}
}

// ScaleEvent creates a scale up/down event
func ScaleEvent(project, env, service string, fromReplicas, toReplicas int) Event {
	eventType := EventScaleUp
	if toReplicas < fromReplicas {
		eventType = EventScaleDown
	}
	return Event{
		Type:        eventType,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Service `%s` scaled from %d to %d replicas", service, fromReplicas, toReplicas),
		Details: map[string]string{
			"from_replicas": fmt.Sprintf("%d", fromReplicas),
			"to_replicas":   fmt.Sprintf("%d", toReplicas),
		},
		Timestamp: time.Now(),
	}
}

// ServiceDownEvent creates a service down event
func ServiceDownEvent(project, env, service string, err error) Event {
	return Event{
		Type:        EventServiceDown,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     fmt.Sprintf("Service `%s` is down", service),
		Error:       err.Error(),
		Timestamp:   time.Now(),
	}
}

// ServiceUpEvent creates a service up/recovered event
func ServiceUpEvent(project, env, service string, downtime time.Duration) Event {
	msg := fmt.Sprintf("Service `%s` is back online", service)
	if downtime > 0 {
		msg = fmt.Sprintf("Service `%s` recovered after %s downtime", service, downtime.Round(time.Second))
	}
	return Event{
		Type:        EventServiceUp,
		Project:     project,
		Environment: env,
		Service:     service,
		Message:     msg,
		Duration:    downtime,
		Timestamp:   time.Now(),
	}
}
