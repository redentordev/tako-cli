package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func TestExecuteCertificateNodeRequestsPreflightsAllNodesBeforeMutation(t *testing.T) {
	stale := &fakeCertificateExecutor{status: `{"capabilities":["proxy.trusted-proxies-v1"]}`}
	current := &fakeCertificateExecutor{status: `{"capabilities":["proxy.certs-v1"]}`, response: `{"certificate":{"domain":"example.com","source":"pushed"}}`}
	nodes := []engine.CertsNodeResult{{Server: "node-a"}, {Server: "node-b"}}
	clients := map[string]takodclient.RequestExecutor{"node-a": stale, "node-b": current}
	_, err := executeCertificateNodeRequests(context.Background(), takodclient.DefaultSocket, nodes, clients, "push", "example.com", &takod.ProxyCertificatePushRequest{Domain: "example.com", CertPEM: "cert", KeyPEM: "private-key"})
	var capabilityErr *takodclient.CapabilityRequiredError
	if !errors.As(err, &capabilityErr) || capabilityErr.Server != "node-a" {
		t.Fatalf("error = %v, want node-a CapabilityRequiredError", err)
	}
	if stale.inputCalls != 0 || current.inputCalls != 0 {
		t.Fatalf("mutation calls stale=%d current=%d, want zero", stale.inputCalls, current.inputCalls)
	}
	if stale.statusCalls != 1 || current.statusCalls != 1 {
		t.Fatalf("status calls stale=%d current=%d, want one each", stale.statusCalls, current.statusCalls)
	}
}

func TestExecuteCertificateNodeRequestsSendsPrivateKeyOnlyInRequestBody(t *testing.T) {
	executor := &fakeCertificateExecutor{status: `{"capabilities":["proxy.certs-v1"]}`, response: `{"certificate":{"domain":"example.com","source":"pushed"}}`}
	nodes, err := executeCertificateNodeRequests(context.Background(), takodclient.DefaultSocket, []engine.CertsNodeResult{{Server: "node-a"}}, map[string]takodclient.RequestExecutor{"node-a": executor}, "push", "example.com", &takod.ProxyCertificatePushRequest{Domain: "example.com", CertPEM: "certificate", KeyPEM: "PRIVATE-KEY-MATERIAL"})
	if err != nil {
		t.Fatalf("executeCertificateNodeRequests returned error: %v", err)
	}
	if executor.inputCalls != 1 || !strings.Contains(executor.body, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("request body was not streamed exactly once: calls=%d body=%q", executor.inputCalls, executor.body)
	}
	if strings.Contains(executor.inputCommand, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("private key leaked into argv: %s", executor.inputCommand)
	}
	if len(nodes[0].Certificates) != 1 || nodes[0].Certificates[0].Domain != "example.com" {
		t.Fatalf("nodes = %#v", nodes)
	}
}

func TestCertificateRejectedInputIsTypedAndEchoedKeyIsRedacted(t *testing.T) {
	const privateKey = "PRIVATE-KEY-MATERIAL"
	executor := &fakeCertificateExecutor{
		status:   `{"capabilities":["proxy.certs-v1"]}`,
		response: `{"error":"invalid keyPem: PRIVATE-KEY-MATERIAL"}` + "\n__TAKO_HTTP_STATUS__:400",
	}
	nodes, err := executeCertificateNodeRequests(context.Background(), takodclient.DefaultSocket, []engine.CertsNodeResult{{Server: "node-a"}}, map[string]takodclient.RequestExecutor{"node-a": executor}, "push", "example.com", &takod.ProxyCertificatePushRequest{Domain: "example.com", CertPEM: "certificate", KeyPEM: privateKey})
	if engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("Classify(%v) = %d, want ClassInvalid", err, engine.Classify(err))
	}
	oldEngine := cliEngineInstance
	buffer := &events.BufferSink{}
	cliEngineInstance = engine.New(engine.Options{Sink: buffer})
	cliEngine().RegisterSecret(privateKey)
	t.Cleanup(func() { cliEngineInstance = oldEngine })
	result := engine.CertsResult{Error: err.Error(), Nodes: nodes}
	err = redactCertificateError(err)
	redactCertsResultErrors(&result)
	payload, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(payload), privateKey) || strings.Contains(err.Error(), privateKey) {
		t.Fatalf("private key leaked after redaction: result=%s err=%v", payload, err)
	}
	if engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("redacted typed error/result = %v %s", err, payload)
	}
}

func TestCertificateOperationEventsGoldenAndRedacted(t *testing.T) {
	const privateKey = "PRIVATE-KEY-MATERIAL"
	oldEngine := cliEngineInstance
	buffer := &events.BufferSink{}
	cliEngineInstance = engine.New(engine.Options{Sink: buffer})
	cliEngine().RegisterSecret(privateKey)
	t.Cleanup(func() { cliEngineInstance = oldEngine })
	for _, action := range []string{"push", "list", "remove"} {
		emitCertificateOperationEvents(engine.CertsResult{Action: action, Domain: "example.com", Nodes: []engine.CertsNodeResult{{Server: "node-a"}}})
	}
	emitted := buffer.Events()
	if len(emitted) != 3 {
		t.Fatalf("events = %#v", emitted)
	}
	for i, event := range emitted {
		if event.Type != events.TypeCertificate || event.Phase != events.PhaseDomains || event.Node != "node-a" || event.Data["action"] != []string{"push", "list", "remove"}[i] || event.Data["domain"] != "example.com" {
			t.Fatalf("event[%d] = %#v", i, event)
		}
		payload, err := json.Marshal(event)
		if err != nil || strings.Contains(string(payload), privateKey) {
			t.Fatalf("event leaked private key: %s err=%v", payload, err)
		}
	}
}

func TestRedactCertificateErrorPreservesConnectivityClassification(t *testing.T) {
	oldEngine := cliEngineInstance
	cliEngineInstance = engine.New(engine.Options{Sink: events.NopSink{}})
	t.Cleanup(func() { cliEngineInstance = oldEngine })
	err := redactCertificateError(&engine.ConnectivityError{Server: "node-a", Err: errors.New("dial failed")})
	if engine.Classify(err) != engine.ClassConnectivity {
		t.Fatalf("Classify(%v) = %d, want ClassConnectivity", err, engine.Classify(err))
	}
}

func TestCertificateTargetServersUsesProxyPlacement(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{"node-a": {}, "node-b": {}, "node-c": {}},
		Environments: map[string]config.EnvironmentConfig{"production": {
			Servers: []string{"node-a", "node-b", "node-c"},
			Proxy:   &config.EnvironmentProxyConfig{Placement: &config.PlacementConfig{Strategy: "pinned", Servers: []string{"node-a", "node-b"}}},
		}},
	}
	servers, err := certificateTargetServers(cfg, "production", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(servers, ",") != "node-a,node-b" {
		t.Fatalf("servers = %v", servers)
	}
	if _, err := certificateTargetServers(cfg, "production", "missing"); err == nil || engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("missing proxy target error = %v", err)
	}
}

type fakeCertificateExecutor struct {
	status       string
	response     string
	statusCalls  int
	inputCalls   int
	body         string
	inputCommand string
}

func (f *fakeCertificateExecutor) ExecuteWithContext(_ context.Context, command string) (string, error) {
	if strings.Contains(command, "/v1/status") {
		f.statusCalls++
		return f.status, nil
	}
	return f.response, nil
}

func (f *fakeCertificateExecutor) ExecuteWithInput(_ context.Context, command string, input io.Reader) (string, error) {
	f.inputCalls++
	f.inputCommand = command
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	f.body = string(data)
	return f.response, nil
}
