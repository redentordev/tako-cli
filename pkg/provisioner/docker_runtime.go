package provisioner

import (
	"encoding/json"
	"fmt"
	"strings"
)

type commandExecutor interface {
	Execute(command string) (string, error)
}

type DockerRuntimeInfo struct {
	ServerVersion   string
	RootDir         string
	SecurityOptions []string
	Rootless        bool
}

const dockerInfoFormat = `{{json .SecurityOptions}}{{"\n"}}{{.DockerRootDir}}{{"\n"}}{{.ServerVersion}}`

func DetectDockerRuntime(client commandExecutor) (*DockerRuntimeInfo, error) {
	output, err := client.Execute("sudo docker info --format " + shellQuote(dockerInfoFormat))
	if err != nil {
		return nil, fmt.Errorf("rootful Docker daemon is not reachable through sudo: %w", err)
	}
	return parseDockerRuntimeInfo(output)
}

func (p *Provisioner) VerifySupportedDockerRuntime() error {
	info, err := DetectDockerRuntime(p.client)
	if err != nil {
		return err
	}
	if info.Rootless {
		return fmt.Errorf("rootless Docker is not supported for takod servers yet; use a rootful system Docker daemon for remote deployment nodes")
	}
	if p.verbose {
		fmt.Printf("  Docker server: %s (root dir: %s)\n", emptyFallback(info.ServerVersion, "unknown"), emptyFallback(info.RootDir, "unknown"))
	}
	return nil
}

func parseDockerRuntimeInfo(output string) (*DockerRuntimeInfo, error) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	if len(lines) < 3 {
		return nil, fmt.Errorf("unexpected docker info output")
	}

	var securityOptions []string
	if raw := strings.TrimSpace(lines[0]); raw != "" && raw != "<no value>" {
		if err := json.Unmarshal([]byte(raw), &securityOptions); err != nil {
			return nil, fmt.Errorf("failed to parse docker security options: %w", err)
		}
	}

	info := &DockerRuntimeInfo{
		SecurityOptions: securityOptions,
		RootDir:         strings.TrimSpace(lines[1]),
		ServerVersion:   strings.TrimSpace(lines[2]),
	}
	for _, option := range securityOptions {
		if strings.Contains(strings.ToLower(option), "rootless") {
			info.Rootless = true
			break
		}
	}
	return info, nil
}

func emptyFallback(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
