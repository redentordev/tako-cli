package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
)

type NodeInfoResponse struct {
	Node            string         `json:"node,omitempty"`
	Hostname        string         `json:"hostname,omitempty"`
	Platform        string         `json:"platform,omitempty"`
	OS              string         `json:"os,omitempty"`
	Architecture    string         `json:"architecture,omitempty"`
	Docker          DockerNodeInfo `json:"docker"`
	BuildxAvailable bool           `json:"buildxAvailable"`
	BuildxVersion   string         `json:"buildxVersion,omitempty"`
}

type DockerNodeInfo struct {
	OSType          string `json:"osType,omitempty"`
	Architecture    string `json:"architecture,omitempty"`
	OperatingSystem string `json:"operatingSystem,omitempty"`
	ServerVersion   string `json:"serverVersion,omitempty"`
	Driver          string `json:"driver,omitempty"`
	CgroupDriver    string `json:"cgroupDriver,omitempty"`
	DefaultRuntime  string `json:"defaultRuntime,omitempty"`
	RootDir         string `json:"rootDir,omitempty"`
}

type dockerInfoOutput struct {
	OSType          string `json:"OSType"`
	Architecture    string `json:"Architecture"`
	OperatingSystem string `json:"OperatingSystem"`
	ServerVersion   string `json:"ServerVersion"`
	Driver          string `json:"Driver"`
	CgroupDriver    string `json:"CgroupDriver"`
	DefaultRuntime  string `json:"DefaultRuntime"`
	DockerRootDir   string `json:"DockerRootDir"`
}

func NodeInfo(ctx context.Context, nodeName string) (*NodeInfoResponse, error) {
	output, err := runDocker(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("failed to read Docker node info: %w", err)
	}

	var dockerInfo dockerInfoOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &dockerInfo); err != nil {
		return nil, fmt.Errorf("failed to parse Docker node info: %w", err)
	}

	osType := normalizeDockerOS(dockerInfo.OSType)
	if osType == "" {
		osType = normalizeDockerOS(runtime.GOOS)
	}
	arch := normalizeDockerArchitecture(dockerInfo.Architecture)
	if arch == "" {
		arch = normalizeDockerArchitecture(runtime.GOARCH)
	}

	hostname, _ := os.Hostname()
	response := &NodeInfoResponse{
		Node:         strings.TrimSpace(nodeName),
		Hostname:     strings.TrimSpace(hostname),
		OS:           osType,
		Architecture: arch,
		Docker: DockerNodeInfo{
			OSType:          strings.TrimSpace(dockerInfo.OSType),
			Architecture:    strings.TrimSpace(dockerInfo.Architecture),
			OperatingSystem: strings.TrimSpace(dockerInfo.OperatingSystem),
			ServerVersion:   strings.TrimSpace(dockerInfo.ServerVersion),
			Driver:          strings.TrimSpace(dockerInfo.Driver),
			CgroupDriver:    strings.TrimSpace(dockerInfo.CgroupDriver),
			DefaultRuntime:  strings.TrimSpace(dockerInfo.DefaultRuntime),
			RootDir:         strings.TrimSpace(dockerInfo.DockerRootDir),
		},
	}
	if response.OS != "" && response.Architecture != "" {
		response.Platform = response.OS + "/" + response.Architecture
	}

	if buildxOutput, err := runDocker(ctx, "buildx", "version"); err == nil {
		response.BuildxAvailable = true
		response.BuildxVersion = strings.TrimSpace(buildxOutput)
	}

	return response, nil
}

func normalizeDockerOS(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "unknown":
		return ""
	case "linux":
		return "linux"
	default:
		return value
	}
}

func normalizeDockerArchitecture(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "linux/")
	switch value {
	case "", "unknown":
		return ""
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64", "arm64/v8":
		return "arm64"
	case "arm/v7", "armv7", "armv7l", "armhf":
		return "arm/v7"
	case "arm/v6", "armv6", "armv6l":
		return "arm/v6"
	case "386", "i386", "i686", "x86":
		return "386"
	default:
		return value
	}
}
