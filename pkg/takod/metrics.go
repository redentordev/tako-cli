package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

var (
	monitorCommandContext = exec.CommandContext
	monitorCommandPath    = "/usr/local/bin/tako-monitor.sh"
	metricsCurrentPath    = "/var/lib/tako/metrics/current.json"
)

type MetricsResponse struct {
	Collected bool            `json:"collected"`
	Metrics   json.RawMessage `json:"metrics"`
}

func ReadNodeMetrics(ctx context.Context, collect bool) (*MetricsResponse, error) {
	if collect {
		cmd := monitorCommandContext(ctx, monitorCommandPath, "once")
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("failed to collect node metrics: %w, output: %s", err, string(output))
		}
	}

	data, err := os.ReadFile(metricsCurrentPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read node metrics: %w", err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("node metrics file is not valid JSON")
	}

	return &MetricsResponse{
		Collected: collect,
		Metrics:   append(json.RawMessage(nil), data...),
	}, nil
}
