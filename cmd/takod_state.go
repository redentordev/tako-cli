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
	gitInfo takodstate.GitInfo,
	eventType string,
	message string,
	details map[string]string,
) error {
	desired, err := takodstate.BuildDesiredRevision(cfg, envName, source, services, imageRefs, serverNames, gitInfo)
	if err != nil {
		return fmt.Errorf("failed to build desired revision: %w", err)
	}
	actual := takodstate.BuildActualSnapshot(cfg.Project.Name, envName, serverNames, actualState)
	if details == nil {
		details = make(map[string]string)
	}
	details["revisionId"] = desired.RevisionID

	event := takodstate.NewEvent(cfg.Project.Name, envName, eventType, desired.RevisionID, message, details)
	return takodstate.PersistToServers(sshPool, cfg, envName, serverNames, desired, actual, event, verbose)
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
