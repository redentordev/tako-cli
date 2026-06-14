package takod

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var meshPingCommandContext = exec.CommandContext

type MeshRTTRequest struct {
	Target string `json:"target"`
	Count  int    `json:"count,omitempty"`
}

type MeshRTTResponse struct {
	Target      string  `json:"target"`
	Count       int     `json:"count"`
	Sent        int     `json:"sent"`
	Received    int     `json:"received"`
	LossPercent float64 `json:"lossPercent"`
	MinMS       float64 `json:"minMs,omitempty"`
	AvgMS       float64 `json:"avgMs,omitempty"`
	MaxMS       float64 `json:"maxMs,omitempty"`
	Reachable   bool    `json:"reachable"`
}

var (
	pingPacketLineRE = regexp.MustCompile(`(?m)(\d+)\s+packets transmitted,\s+(\d+)\s+(?:packets )?received,\s+([0-9.]+)%\s+packet loss`)
	pingRTTLineRE    = regexp.MustCompile(`(?m)(?:rtt|round-trip)[^=]*=\s*([0-9.]+)/([0-9.]+)/([0-9.]+)`)
)

func MeasureMeshRTT(ctx context.Context, req MeshRTTRequest) (*MeshRTTResponse, error) {
	normalizeMeshRTTRequest(&req)
	if err := validateMeshRTTRequest(req); err != nil {
		return nil, err
	}

	cmd := meshPingCommandContext(ctx, "ping", "-c", strconv.Itoa(req.Count), "-W", "2", "-n", req.Target)
	output, err := cmd.CombinedOutput()
	response, parseErr := parsePingOutput(req.Target, req.Count, string(output))
	if parseErr != nil {
		if err != nil {
			return nil, fmt.Errorf("failed to measure mesh RTT: %w, output: %s", err, strings.TrimSpace(string(output)))
		}
		return nil, parseErr
	}
	return response, nil
}

func normalizeMeshRTTRequest(req *MeshRTTRequest) {
	req.Target = strings.TrimSpace(req.Target)
	if req.Count == 0 {
		req.Count = 3
	}
}

func validateMeshRTTRequest(req MeshRTTRequest) error {
	ip := net.ParseIP(req.Target)
	if ip == nil {
		return fmt.Errorf("target must be an IP address")
	}
	if !ip.IsPrivate() {
		return fmt.Errorf("target must be a private mesh IP")
	}
	if req.Count < 1 || req.Count > 10 {
		return fmt.Errorf("count must be between 1 and 10")
	}
	return nil
}

func parsePingOutput(target string, count int, output string) (*MeshRTTResponse, error) {
	packetMatch := pingPacketLineRE.FindStringSubmatch(output)
	if len(packetMatch) != 4 {
		return nil, fmt.Errorf("failed to parse ping packet summary")
	}
	sent, _ := strconv.Atoi(packetMatch[1])
	received, _ := strconv.Atoi(packetMatch[2])
	loss, _ := strconv.ParseFloat(packetMatch[3], 64)
	response := &MeshRTTResponse{
		Target:      target,
		Count:       count,
		Sent:        sent,
		Received:    received,
		LossPercent: loss,
		Reachable:   received > 0,
	}
	rttMatch := pingRTTLineRE.FindStringSubmatch(output)
	if len(rttMatch) == 4 {
		response.MinMS, _ = strconv.ParseFloat(rttMatch[1], 64)
		response.AvgMS, _ = strconv.ParseFloat(rttMatch[2], 64)
		response.MaxMS, _ = strconv.ParseFloat(rttMatch[3], 64)
	}
	return response, nil
}
