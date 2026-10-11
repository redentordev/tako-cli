package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

func healthyHygieneStatus(now time.Time) *takod.HygieneStatus {
	return &takod.HygieneStatus{
		Policy:      takod.HygienePolicy{ImageRetention: 3, MaintenanceIntervalSeconds: 86400},
		Filesystems: []takod.HygieneThreshold{{HygieneFilesystem: takod.HygieneFilesystem{Subject: "root", FreeBytes: 40 << 30}, SoftBytes: 10 << 30, HardBytes: 3 << 30}},
		Logging:     takod.HygieneLoggingStatus{Active: true, Reason: "active"},
		Scopes:      []takod.HygieneScopeRecord{{Project: "shop", Environment: "production"}},
		Runs:        []takod.HygieneRun{{Trigger: takod.HygieneTriggerSchedule, FinishedAt: now.Add(-3 * time.Hour)}},
	}
}

func TestEvaluateDiskHygieneRules(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*takod.HygieneStatus)
		status string
		want   string
	}{
		{"healthy", func(*takod.HygieneStatus) {}, "PASS", "no disk pressure"},
		{"unrotated logs", func(s *takod.HygieneStatus) {
			s.Unrotated = []takod.HygieneUnrotatedContainer{{ID: "a", LogBytes: 700 << 20}, {ID: "b", LogBytes: 700 << 20}}
		}, "WARN", "2 container(s) without log rotation"},
		{"small unrotated logs pass", func(s *takod.HygieneStatus) {
			s.Unrotated = []takod.HygieneUnrotatedContainer{{ID: "a", LogBytes: 10 << 20}}
		}, "PASS", "no disk pressure"},
		{"operator logging", func(s *takod.HygieneStatus) {
			s.Logging = takod.HygieneLoggingStatus{Reason: "daemon.json sets log-opts (operator-managed)"}
		}, "WARN", "default log rotation skipped"},
		{"logging off by flag", func(s *takod.HygieneStatus) {
			s.Logging = takod.HygieneLoggingStatus{Reason: "disabled by --default-container-logging=off"}
		}, "PASS", ""},
		{"unreadable state", func(s *takod.HygieneStatus) {
			s.Runs[0].Skipped = []string{"images shop/production: unreadable state: /var/lib/tako/state/shop/production/history.json"}
		}, "WARN", "unreadable state"},
		{"no scopes", func(s *takod.HygieneStatus) { s.Scopes = nil }, "WARN", "no recorded scope"},
		{"stale run", func(s *takod.HygieneStatus) { s.Runs[0].FinishedAt = now.Add(-72 * time.Hour) }, "WARN", "no hygiene run in the last 48h"},
		{"disabled", func(s *takod.HygieneStatus) {
			s.Runs = nil
			s.Policy.MaintenanceIntervalSeconds = 0
		}, "WARN", "--hygiene-interval=0"},
		{"pressure", func(s *takod.HygieneStatus) { s.Filesystems[0].Level = takod.HygieneLevelSoft }, "WARN", "below the soft threshold"},
		{"insufficient", func(s *takod.HygieneStatus) { s.InsufficientSubjects = []string{"docker"} }, "FAIL", "below the floor after a reclaim on docker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := healthyHygieneStatus(now)
			tc.mutate(status)
			result := evaluateDiskHygiene("app-1", &doctorHygieneProbe{Capable: true, Status: status}, now)
			if result.status != tc.status || !strings.Contains(result.message, tc.want) || !strings.HasPrefix(result.message, "app-1: ") {
				t.Fatalf("result = %+v, want %s containing %q", result, tc.status, tc.want)
			}
		})
	}
}

func TestCheckDiskHygieneOldNodeWarnsToUpgrade(t *testing.T) {
	var results []checkResult
	record := func(result checkResult) { results = append(results, result) }
	checkDiskHygieneWith(record, []string{"new", "old", "down"}, time.Now(), func(name string) (*doctorHygieneProbe, error) {
		switch name {
		case "old":
			return &doctorHygieneProbe{}, nil
		case "down":
			return nil, errors.New("connection refused")
		}
		return &doctorHygieneProbe{Capable: true, Status: healthyHygieneStatus(time.Now())}, nil
	})
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].status != "PASS" {
		t.Fatalf("new node = %+v", results[0])
	}
	if results[1].status != "WARN" || !strings.Contains(results[1].message, "upgrade for disk hygiene") {
		t.Fatalf("old node = %+v", results[1])
	}
	if results[2].status != "WARN" || !strings.Contains(results[2].message, "connection refused") {
		t.Fatalf("unreachable node = %+v", results[2])
	}
}

func TestDoctorDiskHygieneSectionHasStableID(t *testing.T) {
	if doctorSectionIDs[doctorDiskHygieneSection] != "disk-hygiene" {
		t.Fatalf("disk hygiene check id = %q", doctorSectionIDs[doctorDiskHygieneSection])
	}
}
