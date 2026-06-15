package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type StatsRequest struct {
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Service     string `json:"service,omitempty"`
	All         bool   `json:"all,omitempty"`
}

type StatsResponse struct {
	Stats []ContainerStat `json:"stats"`
}

type ContainerStat struct {
	Name       string `json:"name"`
	CPUPercent string `json:"cpuPercent"`
	MemUsage   string `json:"memUsage"`
	MemPercent string `json:"memPercent"`
	NetIO      string `json:"netIO"`
	BlockIO    string `json:"blockIO"`
	PIDs       string `json:"pids"`
}

func ReadContainerStats(ctx context.Context, req StatsRequest) (*StatsResponse, error) {
	if err := validateStatsRequest(req); err != nil {
		return nil, err
	}

	args := []string{"stats", "--no-stream", "--format", "{{json .}}"}
	if !req.All || req.Service != "" {
		containers, err := listStatsContainers(ctx, req)
		if err != nil {
			return nil, err
		}
		if len(containers) == 0 {
			return &StatsResponse{Stats: []ContainerStat{}}, nil
		}
		args = append(args, containers...)
	}

	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read container stats: %w", err)
	}

	stats, err := parseDockerStats(output)
	if err != nil {
		return nil, err
	}
	return &StatsResponse{Stats: stats}, nil
}

func validateStatsRequest(req StatsRequest) error {
	if req.All && req.Service == "" {
		return nil
	}
	for label, value := range map[string]string{
		"project":     req.Project,
		"environment": req.Environment,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if req.Service != "" && !isSafeServiceName(req.Service) {
		return fmt.Errorf("invalid service name")
	}
	return nil
}

func listStatsContainers(ctx context.Context, req StatsRequest) ([]string, error) {
	args := []string{
		"ps",
		"--filter", "label=tako.project=" + req.Project,
		"--filter", "label=tako.environment=" + req.Environment,
	}
	if req.Service != "" {
		args = append(args, "--filter", "label=tako.service="+req.Service)
	}
	args = append(args, "--format", "{{.Names}}")

	output, err := runDocker(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list stats containers: %w", err)
	}
	return strings.Fields(strings.TrimSpace(output)), nil
}

func parseDockerStats(output string) ([]ContainerStat, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	stats := make([]ContainerStat, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var raw map[string]string
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("failed to parse docker stats: %w", err)
		}
		stats = append(stats, ContainerStat{
			Name:       raw["Name"],
			CPUPercent: raw["CPUPerc"],
			MemUsage:   raw["MemUsage"],
			MemPercent: raw["MemPerc"],
			NetIO:      raw["NetIO"],
			BlockIO:    raw["BlockIO"],
			PIDs:       raw["PIDs"],
		})
	}
	return stats, nil
}
