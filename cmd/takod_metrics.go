package cmd

import (
	"fmt"
	"path"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

// takod native metrics flags (add-server-status-history). The defaults turn
// native collection on, so a binary-only `tako upgrade servers` enables it
// without rewriting the systemd unit.
var (
	takodMetricsInterval        time.Duration = takod.DefaultMetricsInterval
	takodMetricsRetention       time.Duration = takod.DefaultMetricsRetention
	takodMetricsHistoryMaxBytes int64         = takod.DefaultMetricsHistoryMaxBytes
	takodMetricsTextfileDir     string
)

func init() {
	flags := takodRunCmd.Flags()
	flags.DurationVar(&takodMetricsInterval, "metrics-interval", takod.DefaultMetricsInterval, "Collect host metrics natively at this interval (0 disables and reads the legacy agent file)")
	flags.DurationVar(&takodMetricsRetention, "metrics-retention", takod.DefaultMetricsRetention, "Keep on-node metrics history this long (1h to 744h)")
	flags.Int64Var(&takodMetricsHistoryMaxBytes, "metrics-history-max-bytes", takod.DefaultMetricsHistoryMaxBytes, "Cap on-node metrics history at this many bytes (oldest files are deleted first)")
	flags.StringVar(&takodMetricsTextfileDir, "metrics-textfile-dir", "", "Write tako_node.prom here after each sample for node_exporter's textfile collector (off by default)")
}

// takodMetricsOptions validates the metrics flags.
func takodMetricsOptions() (takod.MetricsOptions, error) {
	if takodMetricsInterval < 0 {
		return takod.MetricsOptions{}, fmt.Errorf("--metrics-interval must not be negative")
	}
	if takodMetricsInterval > 0 && takodMetricsInterval < 5*time.Second {
		return takod.MetricsOptions{}, fmt.Errorf("--metrics-interval must be at least 5s (or 0 to disable)")
	}
	if takodMetricsRetention < takod.MinimumMetricsRetention || takodMetricsRetention > takod.MaximumMetricsRetention {
		return takod.MetricsOptions{}, fmt.Errorf("--metrics-retention must be between %s and %s", takod.MinimumMetricsRetention, takod.MaximumMetricsRetention)
	}
	if takodMetricsHistoryMaxBytes <= 0 {
		return takod.MetricsOptions{}, fmt.Errorf("--metrics-history-max-bytes must be positive")
	}
	if takodMetricsTextfileDir != "" && !path.IsAbs(takodMetricsTextfileDir) {
		return takod.MetricsOptions{}, fmt.Errorf("--metrics-textfile-dir must be an absolute path")
	}
	return takod.MetricsOptions{
		Interval: takodMetricsInterval, Retention: takodMetricsRetention,
		HistoryMaxBytes: takodMetricsHistoryMaxBytes, TextfileDir: takodMetricsTextfileDir,
	}, nil
}
