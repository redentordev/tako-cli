package config

import (
	"strings"
	"testing"
)

const (
	testTransportClusterID = "11111111-1111-4111-8111-111111111111"
	testTransportNodeID    = "22222222-2222-4222-8222-222222222222"
)

func TestValidateServerRequiresIdentityAndWorkerUIDForLocalTransport(t *testing.T) {
	base := ServerConfig{Host: "node.example.test", User: "deploy", Password: "$SSH_PASSWORD", Transport: "auto"}
	for _, test := range []struct {
		name   string
		mutate func(*ServerConfig)
		want   string
	}{
		{name: "missing identity", want: "requires valid clusterId and nodeId"},
		{name: "missing worker uid", mutate: func(server *ServerConfig) {
			server.ClusterID, server.NodeID = testTransportClusterID, testTransportNodeID
		}, want: "workerUid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := base
			if test.mutate != nil {
				test.mutate(&server)
			}
			err := validateServer("node", &server)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateServer error = %v, want %q", err, test.want)
			}
		})
	}
	valid := base
	valid.ClusterID, valid.NodeID, valid.WorkerUID = testTransportClusterID, testTransportNodeID, 997
	if err := validateServer("node", &valid); err != nil {
		t.Fatalf("valid enrolled server rejected: %v", err)
	}
}

func TestValidateServerPreservesOmittedTransportAsLegacySSH(t *testing.T) {
	server := ServerConfig{Host: "node.example.test", User: "deploy", Password: "$SSH_PASSWORD"}
	if err := validateServer("node", &server); err != nil {
		t.Fatal(err)
	}
	if server.Transport != "" {
		t.Fatalf("legacy transport = %q, want omitted", server.Transport)
	}
}

func TestValidateServerLocalTransportDoesNotRequireSSHCredentials(t *testing.T) {
	server := ServerConfig{Transport: "local", ClusterID: testTransportClusterID, NodeID: testTransportNodeID, WorkerUID: 997}
	if err := validateServer("node", &server); err != nil {
		t.Fatalf("local transport required unrelated SSH configuration: %v", err)
	}
}
