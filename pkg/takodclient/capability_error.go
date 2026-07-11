package takodclient

import (
	"context"
	"encoding/json"
	"fmt"
)

// CapabilityRequiredError rejects an operation before mutation when a target
// node agent does not advertise a capability required by the request.
type CapabilityRequiredError struct {
	Server     string
	Capability string
	Feature    string
}

func (e *CapabilityRequiredError) Error() string {
	return fmt.Sprintf("takod on %s does not support %s; run 'tako upgrade servers' (development builds can set TAKO_TAKOD_BINARY to a matching Linux binary)", e.Server, e.Feature)
}

// RequireCapability reads /v1/status and fails closed before a caller sends a
// payload or invokes an endpoint an older node would silently ignore.
func RequireCapability(ctx context.Context, client RequestExecutor, socket string, server string, required string, feature string) error {
	output, err := RequestJSONWithContext(ctx, client, socket, "GET", "/v1/status", nil)
	if err != nil {
		return fmt.Errorf("failed to verify takod capabilities on %s: %w", server, err)
	}
	var status struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return fmt.Errorf("failed to parse takod status from %s: %w", server, err)
	}
	for _, capability := range status.Capabilities {
		if capability == required {
			return nil
		}
	}
	return &CapabilityRequiredError{Server: server, Capability: required, Feature: feature}
}
