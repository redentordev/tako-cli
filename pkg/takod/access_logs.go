package takod

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

var (
	tailCommandContext = exec.CommandContext
	proxyAccessLogPath = "/var/log/tako/proxy/access.log"
)

func StreamProxyAccessLogs(ctx context.Context, tail int, follow bool, writer io.Writer) error {
	if tail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}
	if tail == 0 {
		tail = 50
	}
	if _, err := os.Stat(proxyAccessLogPath); err != nil {
		return fmt.Errorf("proxy access log not found: %w", err)
	}

	args := []string{"-n", strconv.Itoa(tail)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, proxyAccessLogPath)

	cmd := tailCommandContext(ctx, "tail", args...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stream proxy access logs: %w", err)
	}
	return nil
}
