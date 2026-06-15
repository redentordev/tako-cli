package takod

import (
	"context"
	"os/exec"
	"testing"
)

func TestParsePingOutput(t *testing.T) {
	output := `PING 10.210.0.2 (10.210.0.2) 56(84) bytes of data.
64 bytes from 10.210.0.2: icmp_seq=1 ttl=64 time=0.041 ms

--- 10.210.0.2 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss, time 2002ms
rtt min/avg/max/mdev = 0.037/0.041/0.046/0.004 ms
`
	response, err := parsePingOutput("10.210.0.2", 3, output)
	if err != nil {
		t.Fatalf("parsePingOutput returned error: %v", err)
	}
	if !response.Reachable || response.Sent != 3 || response.Received != 3 || response.AvgMS != 0.041 {
		t.Fatalf("unexpected RTT response: %#v", response)
	}
}

func TestMeasureMeshRTTParsesPingCommandOutput(t *testing.T) {
	oldCommand := meshPingCommandContext
	meshPingCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf '%s\n' '3 packets transmitted, 2 received, 33.333% packet loss, time 2002ms' 'rtt min/avg/max/mdev = 1.000/2.000/3.000/0.100 ms'`)
	}
	t.Cleanup(func() { meshPingCommandContext = oldCommand })

	response, err := MeasureMeshRTT(context.Background(), MeshRTTRequest{Target: "10.210.0.2", Count: 3})
	if err != nil {
		t.Fatalf("MeasureMeshRTT returned error: %v", err)
	}
	if !response.Reachable || response.Received != 2 || response.LossPercent != 33.333 || response.AvgMS != 2 {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestMeasureMeshRTTRejectsPublicTarget(t *testing.T) {
	if _, err := MeasureMeshRTT(context.Background(), MeshRTTRequest{Target: "8.8.8.8"}); err == nil {
		t.Fatal("expected public target to be rejected")
	}
}
