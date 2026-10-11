package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// deployCleanupKeepImages is the per-repository retention of deploy-time
// old-image cleanup; it matches the node's default --image-retention.
const deployCleanupKeepImages = 3

// runPostDeployCleanup runs the automatic cleanup after a successful deploy
// on every deployed server. Old-image removal runs only on nodes that
// advertise node.disk-hygiene-v1: older agents select images without the
// history-aware rules this change relies on, so they get the other cleanup
// steps and one warning per deploy naming them.
func (s *DeploySession) runPostDeployCleanup(ctx context.Context, clientFor func(context.Context, string) (any, error)) {
	e := s.engine
	cfg := s.cfg
	imageRepositories := CleanupImageRepositories(cfg, s.envName, s.services)
	externalVolumes := ExternalVolumeNamesForEnvironment(cfg, s.envName)
	serverNames := make([]string, 0, len(s.servers))
	for serverName := range s.servers {
		serverNames = append(serverNames, serverName)
	}
	sort.Strings(serverNames)
	var skipped []string
	for _, serverName := range serverNames {
		client, err := clientFor(ctx, serverName)
		if err != nil {
			continue
		}
		// Preview image repositories are shared with the base; a preview
		// never prunes them where a declared env runs.
		cleanOldImages := s.cleanOldImagesOn(serverName)
		if cleanOldImages && len(imageRepositories) > 0 {
			capable, capErr := takodHasCapability(ctx, client, cfg, takod.CapabilityNodeDiskHygieneV1)
			if capErr != nil || !capable {
				cleanOldImages = false
				skipped = append(skipped, serverName)
			}
		}
		request := takod.CleanupRequest{
			Project:                cfg.Project.Name,
			Environment:            s.envName,
			ImageRepositories:      imageRepositories,
			ExternalVolumes:        externalVolumes,
			KeepImages:             deployCleanupKeepImages,
			CleanOldImages:         cleanOldImages,
			CleanStoppedContainers: true,
			CleanDanglingImages:    true,
			CleanBuildCache:        true,
			BuildCacheKeepStorage:  takod.DefaultBuildCacheKeepStorage,
		}
		if cleanOldImages {
			request.Trigger = takod.HygieneTriggerDeploy
		}
		response, cleanupErr := CleanupViaTakodContext(ctx, client, cfg, request)
		if cleanupErr != nil {
			e.debug(events.TypeWarning, events.PhaseCleanup, fmt.Sprintf("  Warning: failed to clean %s: %v\n", serverName, cleanupErr))
			continue
		}
		if response != nil {
			for _, warning := range response.Warnings {
				e.debug(events.TypeWarning, events.PhaseCleanup, fmt.Sprintf("  Warning: %s\n", warning))
			}
		}
		e.debug(events.TypeCleanupCompleted, events.PhaseCleanup, fmt.Sprintf("  ✓ Cleaned up %s\n", serverName))
	}
	if len(skipped) > 0 {
		e.warn(events.PhaseCleanup, fmt.Sprintf("  Warning: old-image cleanup skipped on %s: node agent needs `tako upgrade servers`\n", strings.Join(skipped, ", ")))
	}
}

// CleanupNode runs one node's `tako cleanup` request adapted to what the
// node supports. With dryRun, only a node with node.disk-hygiene-v1 is
// asked, with a separate old-image-only dry-run request; other nodes fail
// with an upgrade hint. Without dryRun, a node lacking the capability has
// old-image removal dropped from the request (the other steps still run)
// and a notice names the skip.
func CleanupNode(ctx context.Context, client any, cfg *config.Config, server string, request takod.CleanupRequest, dryRun bool) (*takod.CleanupResponse, []string, error) {
	capable := false
	if dryRun || request.CleanOldImages {
		ok, err := takodHasCapability(ctx, client, cfg, takod.CapabilityNodeDiskHygieneV1)
		if err != nil {
			return nil, nil, err
		}
		capable = ok
	}
	if dryRun {
		if !capable {
			return nil, nil, fmt.Errorf("dry run not supported on %s: node agent needs `tako upgrade servers`", server)
		}
		response, err := CleanupViaTakodContext(ctx, client, cfg, takod.CleanupRequest{
			Project:           request.Project,
			Environment:       request.Environment,
			ImageRepositories: request.ImageRepositories,
			KeepImages:        request.KeepImages,
			CleanOldImages:    true,
			DryRun:            true,
			Trigger:           takod.HygieneTriggerCleanup,
		})
		return response, nil, err
	}
	var notices []string
	if request.CleanOldImages {
		if capable {
			request.Trigger = takod.HygieneTriggerCleanup
		} else {
			request.CleanOldImages = false
			notices = append(notices, fmt.Sprintf("old-image removal skipped on %s: node agent needs `tako upgrade servers`", server))
		}
	}
	response, err := CleanupViaTakodContext(ctx, client, cfg, request)
	return response, notices, err
}
