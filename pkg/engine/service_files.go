package engine

import (
	"context"
	"encoding/json"
	"fmt"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func historyServiceFiles(files []config.ServiceFileConfig) []remotestate.ServiceFileState {
	if len(files) == 0 {
		return nil
	}
	out := make([]remotestate.ServiceFileState, 0, len(files))
	for _, file := range files {
		out = append(out, remotestate.ServiceFileState{Target: file.Target, Secret: file.Secret, Owner: file.Owner})
	}
	return out
}

func prepareServiceFileHashes(d *deployer.Deployer, services map[string]config.ServiceConfig) (map[string]config.ServiceConfig, error) {
	out := CloneServiceMap(services)
	for name, service := range out {
		if len(service.Files) == 0 {
			continue
		}
		_, _, hash, err := d.PrepareServiceFiles(name, &service)
		if err != nil {
			return nil, fmt.Errorf("failed to fingerprint operator files for service %s: %w", name, err)
		}
		service.FilesContentHash = hash
		out[name] = service
	}
	return out, nil
}

func ensureServiceFilesCapability(ctx context.Context, client takodclient.RequestExecutor, socket string) error {
	output, err := takodclient.RequestJSONWithContext(ctx, client, socket, "GET", "/v1/status", nil)
	if err != nil {
		return fmt.Errorf("failed to verify operator file support: %w", err)
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return fmt.Errorf("failed to parse takod status for operator file support: %w", err)
	}
	for _, capability := range status.Capabilities {
		if capability == takod.CapabilityServiceFilesV1 {
			return nil
		}
	}
	return fmt.Errorf("takod does not support operator file distribution; run 'tako upgrade servers'")
}
