package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	gitpkg "github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/reconcile"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

func persistTakodRuntimeState(
	sshPool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serverNames []string,
	source string,
	services map[string]config.ServiceConfig,
	imageRefs map[string]string,
	actualState map[string]*reconcile.ActualService,
	nodeActualState map[string]map[string]*reconcile.ActualService,
	gitInfo takodstate.GitInfo,
	eventType string,
	message string,
	details map[string]string,
) error {
	desired, err := takodstate.BuildDesiredRevision(cfg, envName, source, services, imageRefs, serverNames, gitInfo)
	if err != nil {
		return fmt.Errorf("failed to build desired revision: %w", err)
	}
	actual := takodstate.BuildActualSnapshotWithNodes(cfg.Project.Name, envName, serverNames, actualState, nodeActualState)
	nodeActual := buildNodeActualSnapshots(cfg.Project.Name, envName, nodeActualState)
	if details == nil {
		details = make(map[string]string)
	}
	details["revisionId"] = desired.RevisionID

	event := takodstate.NewEvent(cfg.Project.Name, envName, eventType, desired.RevisionID, message, details)
	return takodstate.PersistToServers(sshPool, cfg, envName, serverNames, desired, actual, nodeActual, event, verbose)
}

func buildNodeActualSnapshots(project string, environment string, nodeActualState map[string]map[string]*reconcile.ActualService) map[string]*takodstate.ActualSnapshot {
	if len(nodeActualState) == 0 {
		return nil
	}
	snapshots := make(map[string]*takodstate.ActualSnapshot, len(nodeActualState))
	for node, actual := range nodeActualState {
		snapshots[node] = takodstate.BuildNodeActualSnapshot(project, environment, node, actual)
	}
	return snapshots
}

func defaultImageRefs(cfg *config.Config, envName string, services map[string]config.ServiceConfig) map[string]string {
	imageRefs := make(map[string]string, len(services))
	for serviceName, service := range services {
		if service.Image != "" {
			imageRefs[serviceName] = service.Image
		} else {
			imageRefs[serviceName] = cfg.GetFullImageName(serviceName, envName)
		}
	}
	return imageRefs
}

func cloneServiceMap(services map[string]config.ServiceConfig) map[string]config.ServiceConfig {
	out := make(map[string]config.ServiceConfig, len(services))
	for name, service := range services {
		out[name] = service
	}
	return out
}

func redactedEnvKeys(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	redacted := make(map[string]string, len(env))
	for key := range env {
		redacted[key] = "<redacted>"
	}
	return redacted
}

func gitInfoFromCommit(commitInfo *gitpkg.CommitInfo) takodstate.GitInfo {
	if commitInfo == nil {
		return takodstate.GitInfo{}
	}
	return takodstate.GitInfo{
		Commit:      commitInfo.Hash,
		CommitShort: commitInfo.ShortHash,
		Branch:      commitInfo.Branch,
		Message:     commitInfo.Message,
		Author:      commitInfo.Author,
	}
}
