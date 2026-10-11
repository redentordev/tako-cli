package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

const (
	// doctorDiskHygieneSection is the doctor section for node disk hygiene;
	// its checks carry the stable id doctorDiskHygieneCheckID.
	doctorDiskHygieneSection = "Disk Hygiene"
	doctorDiskHygieneCheckID = "disk-hygiene"
	// doctorHygieneRunWindow is how recent the last hygiene run must be.
	doctorHygieneRunWindow = 48 * time.Hour
	// doctorUnrotatedLogWarnBytes is the unrotated log total that warns.
	doctorUnrotatedLogWarnBytes = int64(1) << 30
	// doctorHygieneProbeTimeout covers GET /v1/hygiene, which inspects
	// running containers for unrotated logs.
	doctorHygieneProbeTimeout = 90 * time.Second
)

// doctorSectionIDs maps doctor sections to stable machine check ids.
var doctorSectionIDs = map[string]string{
	doctorDiskHygieneSection: doctorDiskHygieneCheckID,
}

// doctorHygieneProbe is one node's hygiene status. Capable is false for a
// node agent without node.disk-hygiene-v1, and Status is then nil.
type doctorHygieneProbe struct {
	Capable bool
	Status  *takod.HygieneStatus
}

func checkDiskHygiene(record func(checkResult), cfg *config.Config, clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check disk hygiene", ""})
		return
	}
	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)
	checkDiskHygieneWith(record, clientNames, time.Now(), func(clientName string) (*doctorHygieneProbe, error) {
		return readDoctorHygiene(clients[clientName], cfg)
	})
}

func readDoctorHygiene(client *ssh.Client, cfg *config.Config) (*doctorHygieneProbe, error) {
	socket := takodSocketFromConfig(cfg)
	output, err := takodclient.RequestJSONWithTimeout(client, socket, "GET", "/v1/status", nil, doctorNodeProbeTimeout)
	if err != nil {
		return nil, err
	}
	var status takod.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return nil, fmt.Errorf("unreadable takod status: %w", err)
	}
	capable := false
	for _, capability := range status.Capabilities {
		if capability == takod.CapabilityNodeDiskHygieneV1 {
			capable = true
			break
		}
	}
	if !capable {
		return &doctorHygieneProbe{}, nil
	}
	output, err = takodclient.RequestJSONWithTimeout(client, socket, "GET", takodclient.HygieneEndpoint, nil, doctorHygieneProbeTimeout)
	if err != nil {
		return nil, err
	}
	var hygiene takod.HygieneStatus
	if err := json.Unmarshal([]byte(output), &hygiene); err != nil {
		return nil, fmt.Errorf("unreadable hygiene status: %w", err)
	}
	return &doctorHygieneProbe{Capable: true, Status: &hygiene}, nil
}

func checkDiskHygieneWith(record func(checkResult), clientNames []string, now time.Time, probe func(string) (*doctorHygieneProbe, error)) {
	for _, clientName := range clientNames {
		result, err := probe(clientName)
		if err != nil || result == nil {
			record(checkResult{"WARN", fmt.Sprintf("%s: Cannot read disk hygiene status: %v", clientName, err), "Check 'systemctl status takod' on the node"})
			continue
		}
		record(evaluateDiskHygiene(clientName, result, now))
	}
}

// evaluateDiskHygiene applies the doctor rules to one node: fail when a
// reclaim left a filesystem below its floor; warn for unrotated logs over
// 1 GiB, a default log rotation skipped by operator policy, a run skipped
// for unreadable state, no recorded scope, pressure or no recent run;
// otherwise pass.
func evaluateDiskHygiene(clientName string, result *doctorHygieneProbe, now time.Time) checkResult {
	if !result.Capable || result.Status == nil {
		return checkResult{"WARN", fmt.Sprintf("%s: node agent has no disk hygiene (upgrade for disk hygiene)", clientName), "Run 'tako upgrade servers'"}
	}
	status := result.Status
	if len(status.InsufficientSubjects) > 0 {
		return checkResult{"FAIL", fmt.Sprintf("%s: free space still below the floor after a reclaim on %s", clientName, strings.Join(status.InsufficientSubjects, ", ")), "Free disk space (volumes, backups and logs are never reclaimed automatically) or grow the disk"}
	}

	var warnings []string
	var unrotatedBytes int64
	for _, container := range status.Unrotated {
		unrotatedBytes += container.LogBytes
	}
	if unrotatedBytes > doctorUnrotatedLogWarnBytes {
		warnings = append(warnings, fmt.Sprintf("%d container(s) without log rotation hold %s of logs", len(status.Unrotated), takodclient.FormatBytes(unrotatedBytes)))
	}
	if !status.Logging.Active && strings.Contains(status.Logging.Reason, "operator-managed") {
		warnings = append(warnings, "default log rotation skipped: "+status.Logging.Reason)
	}
	if len(status.Runs) > 0 {
		for _, skipped := range status.Runs[0].Skipped {
			if strings.Contains(skipped, "unreadable state") {
				warnings = append(warnings, "last run skipped images for unreadable state")
				break
			}
		}
	}
	if len(status.Scopes) == 0 {
		warnings = append(warnings, "no recorded scope (no deploy since the upgrade)")
	}
	for _, filesystem := range status.Filesystems {
		if filesystem.Level != "" {
			warnings = append(warnings, fmt.Sprintf("%s has %s free, below the %s threshold", filesystem.Subject, takodclient.FormatBytes(filesystem.FreeBytes), filesystem.Level))
		}
	}
	recent := false
	for _, run := range status.Runs {
		if !run.FinishedAt.IsZero() && now.Sub(run.FinishedAt) <= doctorHygieneRunWindow {
			recent = true
			break
		}
	}
	if !recent {
		if status.Policy.MaintenanceIntervalSeconds <= 0 {
			warnings = append(warnings, "autonomous hygiene is disabled (--hygiene-interval=0)")
		} else {
			warnings = append(warnings, "no hygiene run in the last 48h")
		}
	}
	if len(warnings) > 0 {
		return checkResult{"WARN", fmt.Sprintf("%s: %s", clientName, strings.Join(warnings, "; ")), "Run 'tako cleanup --dry-run' and see docs/RESOURCE-MONITORING.md (Disk hygiene)"}
	}
	return checkResult{"PASS", fmt.Sprintf("%s: last hygiene run %s ago, no disk pressure", clientName, now.Sub(status.Runs[0].FinishedAt).Round(time.Minute)), ""}
}
