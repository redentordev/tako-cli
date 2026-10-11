package takodclient

import (
	"context"
	"net/url"
	"strconv"
)

const (
	// CapabilityNodeSelfHealingV1 mirrors takod.CapabilityNodeSelfHealingV1.
	CapabilityNodeSelfHealingV1 = "node.self-healing-v1"
	// HealPath is GET/PUT /v1/heal.
	HealPath = "/v1/heal"
	// HealFeature names the capability in upgrade errors.
	HealFeature = "self-healing (missing capability " + CapabilityNodeSelfHealingV1 + ")"
)

// HealEndpoint is GET /v1/heal for one environment. journal >= 0 asks for
// that many recent journal entries (0: none); a negative value uses the node
// default.
func HealEndpoint(project, environment string, journal int) string {
	query := url.Values{}
	query.Set("project", project)
	query.Set("environment", environment)
	if journal >= 0 {
		query.Set("journal", strconv.Itoa(journal))
	}
	return HealPath + "?" + query.Encode()
}

// HealStatus reads one environment's self-healing status after checking
// the capability. The raw JSON document is returned for the caller to
// decode (takodclient does not import takod).
func HealStatus(ctx context.Context, client any, socket, server, project, environment string, journal int) (string, error) {
	if err := RequireCapability(ctx, client, socket, server, CapabilityNodeSelfHealingV1, HealFeature); err != nil {
		return "", err
	}
	return RequestJSONWithContext(ctx, client, socket, "GET", HealEndpoint(project, environment, journal), nil)
}

// HealChange sends a pause, resume or reset (PUT /v1/heal) after checking
// the capability. request is a takod.HealChangeRequest.
func HealChange(ctx context.Context, client any, socket, server string, request any) (string, error) {
	if err := RequireCapability(ctx, client, socket, server, CapabilityNodeSelfHealingV1, HealFeature); err != nil {
		return "", err
	}
	return RequestJSONWithContext(ctx, client, socket, "PUT", HealPath, request)
}
