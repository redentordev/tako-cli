package takod

import (
	"context"
	"strings"
	"testing"
)

func TestReadContainerStatsUsesLabelFilteredContainers(t *testing.T) {
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "demo_production_web_1\n")
	t.Setenv("TAKO_FAKE_STATS_OUTPUT", `{"Name":"demo_production_web_1","CPUPerc":"1.23%","MemUsage":"10MiB / 1GiB","MemPerc":"1.00%","NetIO":"1kB / 2kB","BlockIO":"0B / 0B","PIDs":"12"}`+"\n")

	response, err := ReadContainerStats(context.Background(), StatsRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
	})
	if err != nil {
		t.Fatalf("ReadContainerStats returned error: %v", err)
	}
	if len(response.Stats) != 1 {
		t.Fatalf("expected one stat, got %#v", response.Stats)
	}
	stat := response.Stats[0]
	if stat.Name != "demo_production_web_1" || stat.CPUPercent != "1.23%" || stat.MemPercent != "1.00%" {
		t.Fatalf("unexpected stat: %#v", stat)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) != 2 {
		t.Fatalf("expected list and stats commands, got %#v", entries)
	}
	if !strings.Contains(entries[0], "docker ps") ||
		!strings.Contains(entries[0], "label=tako.project=demo") ||
		!strings.Contains(entries[0], "label=tako.environment=production") ||
		!strings.Contains(entries[0], "label=tako.service=web") {
		t.Fatalf("unexpected stats container discovery command: %q", entries[0])
	}
	if entries[1] != "docker stats --no-stream --format {{json .}} demo_production_web_1" {
		t.Fatalf("unexpected stats command: %q", entries[1])
	}
}

func TestReadContainerStatsAllDoesNotRequireProject(t *testing.T) {
	logPath := t.TempDir() + "/commands.log"
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_STATS_OUTPUT", `{"Name":"external","CPUPerc":"2.00%"}`+"\n")

	response, err := ReadContainerStats(context.Background(), StatsRequest{All: true})
	if err != nil {
		t.Fatalf("ReadContainerStats returned error: %v", err)
	}
	if len(response.Stats) != 1 || response.Stats[0].Name != "external" {
		t.Fatalf("unexpected stats response: %#v", response)
	}

	entries := readCommandLog(t, logPath)
	if len(entries) != 1 || entries[0] != "docker stats --no-stream --format {{json .}}" {
		t.Fatalf("unexpected commands: %#v", entries)
	}
}
