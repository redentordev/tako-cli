package config

import (
	"strings"
	"testing"
)

func TestValidateServiceLoggingNormalizesAndBoundsOptions(t *testing.T) {
	logging := &ServiceLoggingConfig{Driver: " JSON-FILE ", MaxSize: "25M", MaxFiles: 5}
	if err := validateServiceLogging("web", logging); err != nil {
		t.Fatal(err)
	}
	if logging.Driver != "json-file" || logging.MaxSize != "25m" || logging.MaxFiles != 5 {
		t.Fatalf("logging = %#v", logging)
	}
	for _, test := range []struct {
		name    string
		logging ServiceLoggingConfig
		want    string
	}{
		{name: "driver", logging: ServiceLoggingConfig{Driver: "journald", MaxSize: "10m", MaxFiles: 3}, want: "driver"},
		{name: "unicode driver whitespace", logging: ServiceLoggingConfig{Driver: "\u00a0local\u00a0", MaxSize: "10m", MaxFiles: 3}, want: "driver"},
		{name: "unbounded size", logging: ServiceLoggingConfig{Driver: "local", MaxSize: "-1", MaxFiles: 3}, want: "maxSize"},
		{name: "unicode size whitespace", logging: ServiceLoggingConfig{Driver: "local", MaxSize: "\u00a010m\u00a0", MaxFiles: 3}, want: "maxSize"},
		{name: "oversized", logging: ServiceLoggingConfig{Driver: "local", MaxSize: "11g", MaxFiles: 3}, want: "10g"},
		{name: "files", logging: ServiceLoggingConfig{Driver: "local", MaxSize: "10m", MaxFiles: 0}, want: "maxFiles"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateServiceLogging("web", &test.logging); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWorkloadKindsRejectPersistentLoggingControls(t *testing.T) {
	logging := &ServiceLoggingConfig{Driver: "local", MaxSize: "10m", MaxFiles: 3}
	job := &ServiceConfig{Kind: ServiceKindJob, Schedule: "@hourly", Command: ListValue("true"), Logging: logging}
	if err := validateServiceKind("report", job); err == nil || !strings.Contains(err.Error(), "run history") {
		t.Fatalf("job error = %v", err)
	}
	run := &ServiceConfig{Kind: ServiceKindRun, Image: "busybox", Command: ListValue("true"), Logging: logging}
	if err := validateServiceKind("migrate", run); err == nil || !strings.Contains(err.Error(), "no persistent Docker logs") {
		t.Fatalf("run error = %v", err)
	}
}
