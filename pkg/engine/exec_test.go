package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func execTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-a": {Host: "203.0.113.10", User: "deploy"},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"node-a"},
				Services: map[string]config.ServiceConfig{
					"web": {Image: "registry.example.com/web:abc"},
				},
			},
		},
	}
}

func TestExecRejectsInvalidRequests(t *testing.T) {
	e := New(Options{})
	cases := []struct {
		name string
		req  ExecRequest
		want string
	}{
		{"nil config", ExecRequest{Environment: "production", Service: "web", Command: []string{"env"}}, "loaded config"},
		{"missing environment", ExecRequest{Config: execTestConfig(), Service: "web", Command: []string{"env"}}, "environment"},
		{"missing service", ExecRequest{Config: execTestConfig(), Environment: "production", Command: []string{"env"}}, "service"},
		{"missing command", ExecRequest{Config: execTestConfig(), Environment: "production", Service: "web"}, "command"},
		{"unknown service", ExecRequest{Config: execTestConfig(), Environment: "production", Service: "worker", Command: []string{"env"}}, "not found"},
		{"negative replica", ExecRequest{Config: execTestConfig(), Environment: "production", Service: "web", Replica: -1, Command: []string{"env"}}, "replica"},
		{"replica with oneoff", ExecRequest{Config: execTestConfig(), Environment: "production", Service: "web", Replica: 2, OneOff: true, Command: []string{"env"}}, "--oneoff"},
	}
	for _, tc := range cases {
		_, err := e.Exec(context.Background(), tc.req)
		if err == nil {
			t.Fatalf("%s: exec accepted", tc.name)
		}
		var invalid *InvalidRequestError
		if !errors.As(err, &invalid) {
			t.Fatalf("%s: error %v is not InvalidRequestError", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q missing %q", tc.name, err, tc.want)
		}
	}
}

func TestRemoteExitErrorCarriesCode(t *testing.T) {
	err := &RemoteExitError{Code: 42}
	if !strings.Contains(err.Error(), "42") {
		t.Fatalf("error = %q, want code", err)
	}
}
