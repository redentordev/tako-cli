package engine

import (
	"context"
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// connectRuntimeNode returns only the structured runtime surface. The owned
// SSH pool is used only when the pre-resolved transport is SSH.
func connectRuntimeNode(ctx context.Context, cfg *config.Config, serverName string) (*takodclient.AgentClient, func(), error) {
	if cfg == nil {
		return nil, func() {}, fmt.Errorf("runtime connection requires a loaded config")
	}
	pool := ssh.NewPool()
	factory, err := nodeclient.NewFactory(cfg, pool, TakodSocketFromConfig(cfg))
	if err != nil {
		pool.CloseAll()
		return nil, func() {}, err
	}
	client, _, err := factory.Client(ctx, serverName)
	if err != nil {
		factory.CloseIdleConnections()
		pool.CloseAll()
		return nil, func() {}, err
	}
	cleanup := func() {
		factory.CloseIdleConnections()
		pool.CloseAll()
	}
	return client, cleanup, nil
}
