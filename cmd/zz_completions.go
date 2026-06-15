package cmd

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

func init() {
	registerDynamicCompletions()
}

func registerDynamicCompletions() {
	rootCmd.ValidArgsFunction = noPositionalCompletion
	_ = rootCmd.RegisterFlagCompletionFunc("env", completeEnvironments)

	for _, command := range []*cobra.Command{
		execCmd,
		runCmd,
		proxyCmd,
		discoveryCmd,
		inspectCmd,
		psCmd,
	} {
		command.ValidArgsFunction = completeServiceArg
	}
	scaleCmd.ValidArgsFunction = completeScaleArg
	nodeLogsCmd.ValidArgsFunction = completeNodeArg
	imageRemoveCmd.ValidArgsFunction = completeImageArg
	volumeRemoveCmd.ValidArgsFunction = completeVolumeArg

	for _, command := range []*cobra.Command{
		execCmd,
		runCmd,
		proxyCmd,
		discoveryCmd,
		inspectCmd,
		psCmd,
		metricsCmd,
		meshRTTCmd,
		statePullCmd,
		stateStatusCmd,
		stateRepairCmd,
		stateLeaseCmd,
		stateLeaseReleaseCmd,
		historyCmd,
		setupCmd,
		imageListCmd,
		imageRemoveCmd,
		imagePruneCmd,
		volumeListCmd,
		volumeRemoveCmd,
		volumeBackupCmd,
		volumeBackupsCmd,
		volumeRestoreCmd,
		volumeBackupDeleteCmd,
	} {
		registerServerFlagCompletion(command)
	}
	registerStringFlagCompletion(logsCmd, "service", completeServicesForFlag)
	registerStringFlagCompletion(rollbackCmd, "service", completeServicesForFlag)
}

func noPositionalCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func completeEnvironments(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := completionConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Environments))
	for name := range cfg.Environments {
		names = append(names, name)
	}
	return filterCompletions(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeServiceArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveDefault
	}
	return completeServicesForFlag(cmd, args, toComplete)
}

func completeServicesForFlag(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	services, err := completionServiceNames()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterCompletions(services, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeScaleArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if strings.Contains(toComplete, "=") {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	services, err := completionServiceNames()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	used := map[string]bool{}
	for _, arg := range args {
		service, _, ok := strings.Cut(arg, "=")
		if ok {
			used[service] = true
		}
	}
	var out []string
	for _, service := range services {
		if !used[service] {
			out = append(out, service+"=")
		}
	}
	return filterCompletions(out, toComplete), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}

func completeNodeArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeServersForFlag(cmd, args, toComplete)
}

func completeServersForFlag(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := completionConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	envName := getEnvironmentName(cfg)
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterCompletions(serverNames, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeImageArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	images := completeRemoteResources(cmd, "server", func(client takodclient.RequestExecutor, cfg *config.Config, envName string) ([]string, error) {
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.ImagesEndpoint(cfg.Project.Name, envName), nil)
		if err != nil {
			return nil, err
		}
		var response takod.ImageListResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, err
		}
		names := make([]string, 0, len(response.Images))
		for _, image := range response.Images {
			names = append(names, image.Reference)
		}
		return names, nil
	})
	return filterUnusedCompletions(images, args, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeVolumeArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	volumes := completeRemoteResources(cmd, "server", func(client takodclient.RequestExecutor, cfg *config.Config, envName string) ([]string, error) {
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.VolumesEndpoint(cfg.Project.Name, envName), nil)
		if err != nil {
			return nil, err
		}
		var response takod.VolumeListResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return nil, err
		}
		names := make([]string, 0, len(response.Volumes))
		for _, volume := range response.Volumes {
			names = append(names, volume.Name)
		}
		return names, nil
	})
	return filterUnusedCompletions(volumes, args, toComplete), cobra.ShellCompDirectiveNoFileComp
}

type remoteCompletionFunc func(client takodclient.RequestExecutor, cfg *config.Config, envName string) ([]string, error)

func completeRemoteResources(cmd *cobra.Command, serverFlag string, list remoteCompletionFunc) []string {
	cfg, err := completionConfig()
	if err != nil {
		return nil
	}
	envName := getEnvironmentName(cfg)
	requestedServer := completionFlagValue(cmd, serverFlag)
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()

	seen := map[string]bool{}
	var out []string
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			continue
		}
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			continue
		}
		values, err := list(client, cfg, envName)
		if err != nil {
			continue
		}
		for _, value := range values {
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func completionServiceNames() ([]string, error) {
	cfg, err := completionConfig()
	if err != nil {
		return nil, err
	}
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func completionConfig() (*config.Config, error) {
	oldStderr := os.Stderr
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err == nil {
		os.Stderr = devNull
		defer func() {
			os.Stderr = oldStderr
			_ = devNull.Close()
		}()
	}
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func registerServerFlagCompletion(command *cobra.Command) {
	registerStringFlagCompletion(command, "server", completeServersForFlag)
}

func registerStringFlagCompletion(command *cobra.Command, name string, completion cobra.CompletionFunc) {
	if command == nil || command.Flags().Lookup(name) == nil {
		return
	}
	_ = command.RegisterFlagCompletionFunc(name, completion)
}

func completionFlagValue(cmd *cobra.Command, name string) string {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return ""
	}
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return ""
	}
	return value
}

func filterUnusedCompletions(values []string, used []string, prefix string) []string {
	seen := map[string]bool{}
	for _, value := range used {
		seen[value] = true
	}
	var out []string
	for _, value := range values {
		if !seen[value] {
			out = append(out, value)
		}
	}
	return filterCompletions(out, prefix)
}

func filterCompletions(values []string, prefix string) []string {
	var out []string
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
