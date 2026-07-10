package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStringOrListPreservesScalarAndExecForms(t *testing.T) {
	type document struct {
		Command StringOrList `yaml:"command" json:"command"`
	}
	for _, tt := range []struct {
		name       string
		yaml       string
		wantArgs   []string
		wantDocker []string
		wantList   bool
	}{
		{name: "scalar", yaml: "command: npm run worker\n", wantArgs: []string{"npm run worker"}, wantDocker: []string{"sh", "-c", "npm run worker"}},
		{name: "list", yaml: "command: [npm, run, worker]\n", wantArgs: []string{"npm", "run", "worker"}, wantDocker: []string{"npm", "run", "worker"}, wantList: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got document
			if err := yaml.Unmarshal([]byte(tt.yaml), &got); err != nil {
				t.Fatalf("yaml.Unmarshal returned error: %v", err)
			}
			if got.Command.IsList() != tt.wantList || !reflect.DeepEqual(got.Command.Arguments(), tt.wantArgs) {
				t.Fatalf("decoded command = list:%v args:%#v", got.Command.IsList(), got.Command.Arguments())
			}
			if !reflect.DeepEqual(got.Command.ContainerCommand(), tt.wantDocker) {
				t.Fatalf("container command = %#v, want %#v", got.Command.ContainerCommand(), tt.wantDocker)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal returned error: %v", err)
			}
			var roundTrip document
			if err := json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatalf("json.Unmarshal returned error: %v", err)
			}
			if roundTrip.Command.IsList() != tt.wantList || !reflect.DeepEqual(roundTrip.Command.Arguments(), tt.wantArgs) {
				t.Fatalf("round trip command = list:%v args:%#v", roundTrip.Command.IsList(), roundTrip.Command.Arguments())
			}
		})
	}
}

func TestStringOrListUnsetOmitsFromJSON(t *testing.T) {
	data, err := json.Marshal(struct {
		Command StringOrList `json:"command,omitempty,omitzero"`
	}{})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if string(data) != "{}" {
		t.Fatalf("unset value encoded as %s, want {}", data)
	}
}

func TestValidateConfigAcceptsContainerGapAFields(t *testing.T) {
	cfg := validValidationConfig()
	production := cfg.Environments["production"]
	web := production.Services["web"]
	web.Command = ListValue("node", "worker.js")
	web.Entrypoint = ListValue("/usr/bin/env", "node")
	web.Labels = map[string]string{"com.example.role": "worker"}
	web.HealthCheck = HealthCheckConfig{Command: "node healthcheck.js"}
	production.Services["web"] = web
	cfg.Environments["production"] = production

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig returned error: %v", err)
	}
	got := cfg.Environments["production"].Services["web"]
	if got.HealthCheck.Interval != "10s" || got.HealthCheck.Timeout != "5s" || got.HealthCheck.Retries != 3 {
		t.Fatalf("health defaults = %#v", got.HealthCheck)
	}
}

func TestValidateConfigRejectsReservedContainerLabelAndInvalidArgv(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*ServiceConfig)
		want   string
	}{
		{name: "reserved label", mutate: func(service *ServiceConfig) { service.Labels = map[string]string{"tako.project": "other"} }, want: "reserved tako. prefix"},
		{name: "empty command list", mutate: func(service *ServiceConfig) { service.Command = ListValue() }, want: "command must not be empty"},
		{name: "entrypoint control", mutate: func(service *ServiceConfig) { service.Entrypoint = StringValue("bad\nentrypoint") }, want: "entrypoint contains control"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validValidationConfig()
			production := cfg.Environments["production"]
			service := production.Services["web"]
			tt.mutate(&service)
			production.Services["web"] = service
			cfg.Environments["production"] = production
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateConfigHealthCommandSizeMatchesTakod(t *testing.T) {
	for _, tt := range []struct {
		name    string
		length  int
		wantErr bool
	}{
		{name: "boundary", length: maxContainerHealthBytes},
		{name: "oversized", length: maxContainerHealthBytes + 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validValidationConfig()
			production := cfg.Environments["production"]
			service := production.Services["web"]
			service.HealthCheck.Command = strings.Repeat("x", tt.length)
			production.Services["web"] = service
			cfg.Environments["production"] = production
			err := ValidateConfig(cfg)
			if tt.wantErr && (err == nil || !strings.Contains(err.Error(), "too large")) {
				t.Fatalf("error = %v, want size rejection", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateConfig returned error at boundary: %v", err)
			}
		})
	}
}
