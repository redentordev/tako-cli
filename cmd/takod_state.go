package cmd

import (
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
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
	return engine.PersistTakodRuntimeState(sshPool, cfg, envName, serverNames, source, services, imageRefs, actualState, nodeActualState, gitInfo, eventType, message, details, verbose)
}

func buildNodeActualSnapshots(project string, environment string, nodeActualState map[string]map[string]*reconcile.ActualService) map[string]*takodstate.ActualSnapshot {
	return engine.BuildNodeActualSnapshots(project, environment, nodeActualState)
}

func cloneServiceMap(services map[string]config.ServiceConfig) map[string]config.ServiceConfig {
	return engine.CloneServiceMap(services)
}

func redactedEnvKeys(env map[string]string) map[string]string {
	return engine.RedactedEnvKeys(env)
}

func gitInfoFromCommit(commitInfo *gitpkg.CommitInfo) takodstate.GitInfo {
	return engine.GitInfoFromCommit(commitInfo)
}
