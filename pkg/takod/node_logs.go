package takod

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

var nodeLogsCommandContext = exec.CommandContext

type NodeLogsRequest struct {
	Unit   string `json:"unit,omitempty"`
	Tail   int    `json:"tail,omitempty"`
	Follow bool   `json:"follow,omitempty"`
}

const maxNodeLogTail = 10000

func StreamNodeLogs(ctx context.Context, req NodeLogsRequest, writer io.Writer) error {
	normalizeNodeLogsRequest(&req)
	if err := validateNodeLogsRequest(req); err != nil {
		return err
	}

	args := []string{"-u", req.Unit, "--no-pager", "-n", strconv.Itoa(req.Tail)}
	if req.Follow {
		args = append(args, "-f")
	}
	cmd := nodeLogsCommandContext(ctx, "journalctl", args...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stream node logs for %s: %w", req.Unit, err)
	}
	return nil
}

func normalizeNodeLogsRequest(req *NodeLogsRequest) {
	req.Unit = strings.TrimSpace(req.Unit)
	if req.Unit == "" {
		req.Unit = "takod"
	}
	if req.Tail == 0 {
		req.Tail = 100
	}
}

func validateNodeLogsRequest(req NodeLogsRequest) error {
	switch req.Unit {
	case "takod", "tako-monitor":
	default:
		return fmt.Errorf("unsupported node log unit")
	}
	if req.Tail < 0 {
		return fmt.Errorf("tail cannot be negative")
	}
	if req.Tail > maxNodeLogTail {
		return fmt.Errorf("tail cannot exceed %d", maxNodeLogTail)
	}
	return nil
}
