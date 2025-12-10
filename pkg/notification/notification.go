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
	EventDriftDetected   EventType = "drift_detected"
	EventBackupCompleted EventType = "backup_completed"
	EventBackupFailed    EventType = "backup_failed"
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
	case EventDeploySucceeded, EventServiceUp, EventBackupCompleted, EventRollbackDone:
		return "#36a64f" // Green
	case EventDeployFailed, EventServiceDown, EventBackupFailed:
		return "#dc3545" // Red
	case EventDeployStarted, EventRollbackStarted:
		return "#007bff" // Blue
	case EventDriftDetected:
		return "#ffc107" // Yellow/Warning
	default:
		return "#6c757d" // Gray
	}
}

// getEventColorInt returns an integer color for Discord
func (n *Notifier) getEventColorInt(eventType EventType) int {
	switch eventType {
	case EventDeploySucceeded, EventServiceUp, EventBackupCompleted, EventRollbackDone:
		return 0x36a64f // Green
	case EventDeployFailed, EventServiceDown, EventBackupFailed:
		return 0xdc3545 // Red
	case EventDeployStarted, EventRollbackStarted:
		return 0x007bff // Blue
	case EventDriftDetected:
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
	case EventDriftDetected:
		return "⚠️"
	case EventBackupCompleted:
		return "💾"
	case EventBackupFailed:
		return "⚠️"
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
	case EventDriftDetected:
		return "Configuration Drift Detected"
	case EventBackupCompleted:
		return "Backup Completed"
	case EventBackupFailed:
		return "Backup Failed"
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
