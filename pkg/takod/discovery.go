package takod

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type ExportDiscoveryRecord struct {
	Network     string `json:"network"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Alias       string `json:"alias"`
	Runtime     string `json:"runtime,omitempty"`
}

type ExportDiscoveryResponse struct {
	Exports []ExportDiscoveryRecord `json:"exports"`
}

func ListExportDiscovery(ctx context.Context, environment string) (*ExportDiscoveryResponse, error) {
	environment = strings.TrimSpace(environment)
	if environment != "" && !isSafeRuntimeName(environment) {
		return nil, fmt.Errorf("invalid environment name")
	}

	output, err := runDocker(ctx, "network", "ls", "--filter", "label=tako.discovery=export", "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("failed to list export discovery networks: %w", err)
	}

	seen := make(map[string]bool)
	records := make([]ExportDiscoveryRecord, 0)
	for _, line := range strings.Split(output, "\n") {
		network := strings.TrimSpace(line)
		if network == "" || seen[network] {
			continue
		}
		seen[network] = true

		labelsRaw, err := runDocker(ctx, "network", "inspect", network, "--format", "{{json .Labels}}")
		if err != nil {
			return nil, fmt.Errorf("failed to inspect export discovery network %s: %w", network, err)
		}
		record, ok := exportDiscoveryRecordFromLabels(network, parseDockerLabels(labelsRaw))
		if !ok {
			continue
		}
		if environment != "" && record.Environment != environment {
			continue
		}
		records = append(records, record)
	}

	sort.Slice(records, func(i, j int) bool {
		left := records[i]
		right := records[j]
		for _, cmp := range []int{
			strings.Compare(left.Environment, right.Environment),
			strings.Compare(left.Project, right.Project),
			strings.Compare(left.Service, right.Service),
			strings.Compare(left.Network, right.Network),
		} {
			if cmp < 0 {
				return true
			}
			if cmp > 0 {
				return false
			}
		}
		return false
	})

	return &ExportDiscoveryResponse{Exports: records}, nil
}

func exportDiscoveryRecordFromLabels(network string, labels map[string]string) (ExportDiscoveryRecord, bool) {
	if labels["tako.discovery"] != "export" {
		return ExportDiscoveryRecord{}, false
	}

	record := ExportDiscoveryRecord{
		Network:     strings.TrimSpace(network),
		Project:     strings.TrimSpace(labels["tako.project"]),
		Environment: strings.TrimSpace(labels["tako.environment"]),
		Service:     strings.TrimSpace(labels["tako.service"]),
		Alias:       strings.TrimSpace(labels["tako.export.alias"]),
		Runtime:     strings.TrimSpace(labels["tako.runtime"]),
	}
	if record.Network == "" || hasControlChars(record.Network) {
		return ExportDiscoveryRecord{}, false
	}
	if !isSafeProjectName(record.Project) {
		return ExportDiscoveryRecord{}, false
	}
	if !isSafeRuntimeName(record.Environment) {
		return ExportDiscoveryRecord{}, false
	}
	if !isSafeServiceName(record.Service) {
		return ExportDiscoveryRecord{}, false
	}
	if record.Alias == "" || len(record.Alias) > 253 || hasControlChars(record.Alias) {
		return ExportDiscoveryRecord{}, false
	}
	if record.Runtime != "" && (len(record.Runtime) > 63 || hasControlChars(record.Runtime)) {
		return ExportDiscoveryRecord{}, false
	}
	return record, true
}
