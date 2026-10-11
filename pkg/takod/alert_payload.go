package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/notification"
)

// AlertPayloadSchemaVersion is the version of the alert webhook body shared
// by scheduler (job, backup) and source (service, host, container,
// certificate, and sibling) deliveries. New keys are additive; existing keys
// keep their meaning.
const AlertPayloadSchemaVersion = 1

// Alert transitions carried in the payload "transition" key. Job and backup
// subjects only use opening and recovery.
const (
	AlertTransitionOpening    = "opening"
	AlertTransitionRecovery   = "recovery"
	AlertTransitionEscalation = "escalation"
	AlertTransitionEvent      = "event"
	AlertTransitionSuppressed = "suppressed"
)

// alertPayloadV1 is the credential-free schema v1 webhook body. Every field
// comes from a closed vocabulary, a public name, a timestamp or a number;
// error strings, response bodies, headers and environment values are never
// copied into it.
type alertPayloadV1 struct {
	EventID     string
	EpisodeID   string
	Transition  string
	Event       string
	Project     string
	Environment string
	Node        string
	Kind        string
	Subject     string
	Severity    string
	Reasons     []string
	Details     map[string]string
	OpenedAt    time.Time
	Message     string
	Timestamp   time.Time
}

// Map renders the payload as the JSON object posted to webhook targets.
// "service" is kept as an alias of "subject" for compatibility.
func (p alertPayloadV1) Map() map[string]any {
	details := make(map[string]string, len(p.Details))
	for key, value := range p.Details {
		details[key] = value
	}
	reasons := append([]string{}, p.Reasons...)
	payload := map[string]any{
		"schemaVersion": AlertPayloadSchemaVersion,
		"eventId":       p.EventID,
		"episodeId":     p.EpisodeID,
		"transition":    p.Transition,
		"event":         p.Event,
		"project":       p.Project,
		"environment":   p.Environment,
		"node":          p.Node,
		"kind":          p.Kind,
		"subject":       p.Subject,
		"service":       p.Subject,
		"severity":      p.Severity,
		"reasons":       reasons,
		"details":       details,
		"message":       p.Message,
		"timestamp":     p.Timestamp,
	}
	if !p.OpenedAt.IsZero() {
		payload["openedAt"] = p.OpenedAt.UTC()
	}
	return payload
}

// schedulerAlertEvent maps legacy scheduler alert kinds to their payload
// event name.
func schedulerAlertEvent(kind string) string {
	switch kind {
	case "backup":
		return string(notification.EventBackupStale)
	default:
		return string(notification.EventJobAlert)
	}
}

// deliverAlertHTTP posts one alert payload through client. Slack and Discord
// receive only the human message; webhook and per-service webhook targets
// receive the full schema v1 body. Results carry only generic error tokens.
func deliverAlertHTTP(ctx context.Context, client *http.Client, provider, endpoint string, payload map[string]any) alertDeliveryResult {
	if endpoint == "" {
		return alertDeliveryResult{Error: "disabled"}
	}
	var body any = payload
	switch provider {
	case "slack":
		body = map[string]any{"text": payload["message"]}
	case "discord":
		body = map[string]any{"content": payload["message"]}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return alertDeliveryResult{Error: "encode"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return alertDeliveryResult{Error: "request"}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return alertDeliveryResult{Interrupted: true, Error: "interrupted"}
		}
		return alertDeliveryResult{Retryable: true, Error: "network"}
	}
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return alertDeliveryResult{Delivered: true}
	}
	retryable := response.StatusCode == 408 || response.StatusCode == 425 || response.StatusCode == 429 || response.StatusCode >= 500
	return alertDeliveryResult{Retryable: retryable, Error: fmt.Sprintf("http_%d", response.StatusCode)}
}

// AlertPayload is the exported schema v1 payload builder used by
// `tako monitor --local`, so the outside checker sends the same body as
// takod.
type AlertPayload struct {
	EventID, EpisodeID, Transition, Event                        string
	Project, Environment, Node, Kind, Subject, Severity, Message string
	Reasons                                                      []string
	Details                                                      map[string]string
	OpenedAt, Timestamp                                          time.Time
}

// Map renders the payload as the webhook JSON object.
func (p AlertPayload) Map() map[string]any {
	return alertPayloadV1{
		EventID: p.EventID, EpisodeID: p.EpisodeID, Transition: p.Transition, Event: p.Event,
		Project: p.Project, Environment: p.Environment, Node: p.Node, Kind: p.Kind, Subject: p.Subject,
		Severity: p.Severity, Reasons: p.Reasons, Details: p.Details, OpenedAt: p.OpenedAt, Message: p.Message, Timestamp: p.Timestamp,
	}.Map()
}

// NewAlertHTTPClient returns the SSRF-safe alert delivery client: public
// addresses only, no proxy, no redirects.
func NewAlertHTTPClient() *http.Client { return newAlertHTTPClient() }

// DeliverAlert posts payload to a validated target through client and
// returns whether it was delivered plus a generic error token.
func DeliverAlert(ctx context.Context, client *http.Client, provider, endpoint string, payload map[string]any) (bool, string) {
	result := deliverAlertHTTP(ctx, client, provider, endpoint, payload)
	return result.Delivered, result.Error
}

// NewAlertID returns a random event or episode identifier.
func NewAlertID() string { return newAlertID() }

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
