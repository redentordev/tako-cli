package stateclient

import (
	"context"
	"fmt"

	"github.com/redentordev/tako-cli/pkg/takoapi"
)

// ReplicateDeployment writes a deployment record followed by its deployment
// history using the private takod state transport.
func (c *Client) ReplicateDeployment(deployment takoapi.DeploymentStateDocument, history *takoapi.DeploymentHistoryDocument) error {
	return c.ReplicateDeploymentContext(context.Background(), deployment, history)
}

// ReplicateDeploymentContext writes a deployment record followed by its
// deployment history bounded by ctx. The deployment record is written first so
// peers can read the individual deployment even if the optional history write is
// skipped. If history is nil, only the deployment record is written.
func (c *Client) ReplicateDeploymentContext(ctx context.Context, deployment takoapi.DeploymentStateDocument, history *takoapi.DeploymentHistoryDocument) error {
	if err := c.WriteDeploymentContext(ctx, deployment); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if history == nil {
		return nil
	}
	if err := c.WriteHistoryContext(ctx, *history); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	return nil
}
