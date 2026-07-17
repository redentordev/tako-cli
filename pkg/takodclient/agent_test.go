package takodclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/upgradeprotocol"
)

type pipeUnixDialer struct {
	mu      sync.Mutex
	paths   []string
	handler func(*http.Request) (int, http.Header, string)
}

func (d *pipeUnixDialer) DialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	client, server := net.Pipe()
	d.mu.Lock()
	d.paths = append(d.paths, path)
	d.mu.Unlock()
	go func() {
		defer server.Close()
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		status, headers, body := d.handler(request)
		if headers == nil {
			headers = make(http.Header)
		}
		response := &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		_ = response.Write(server)
	}()
	return client, nil
}

func (d *pipeUnixDialer) Paths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.paths...)
}

func TestAgentClientRequestJSONUsesStructuredUnixSocketHTTP(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(request *http.Request) (int, http.Header, string) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/test" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if !bytes.Contains(data, []byte(`"name": "demo"`)) {
			t.Errorf("request body = %q", data)
		}
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, `{"ok":true}`
	}}
	client, err := NewAgentClient(dialer, "/run/tako/custom.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	output, err := client.RequestJSON(context.Background(), http.MethodPost, "/v1/test", map[string]string{"name": "demo"})
	if err != nil {
		t.Fatalf("RequestJSON returned error: %v", err)
	}
	if output != `{"ok":true}` {
		t.Fatalf("output = %q", output)
	}
	if got := dialer.Paths(); len(got) != 1 || got[0] != "/run/tako/custom.sock" {
		t.Fatalf("dial paths = %v", got)
	}
}

func TestAgentClientUpgradeDialCannotBypassBoundIngressSocket(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, `{}`
	}}
	client, err := NewAgentClient(dialer, DefaultWorkerSocket)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := client.DialUnixSocket(context.Background(), DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if got := dialer.Paths(); len(got) != 1 || got[0] != DefaultWorkerSocket {
		t.Fatalf("upgrade dial paths = %v, want only protected worker ingress", got)
	}
}

func TestAgentClientDoesNotGloballyCapResponseHeaderWait(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		time.Sleep(25 * time.Millisecond)
		return http.StatusOK, nil, `{"ok":true}`
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	if client.transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("global response header timeout = %s; long operations must use their explicit request context", client.transport.ResponseHeaderTimeout)
	}
	if _, err := client.RequestJSONWithTimeout(context.Background(), http.MethodGet, "/v1/slow", nil, time.Second); err != nil {
		t.Fatalf("long-running structured request failed within its explicit deadline: %v", err)
	}
}

func TestAgentClientStatusValidatesInstallationIdentity(t *testing.T) {
	identity, err := nodeidentity.New(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"node-1",
		[]string{nodeidentity.RoleWorker},
		time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	statusBody, err := json.Marshal(AgentStatus{Runtime: "takod", Version: "test", UpgradeProtocol: upgradeprotocol.Current, MinimumUpgradeProtocol: upgradeprotocol.Current, Capabilities: []string{nodeidentity.Capability}, Identity: &identity.Identity})
	if err != nil {
		t.Fatal(err)
	}
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, string(statusBody)
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Identity == nil || !status.Identity.Matches(identity.ClusterID, identity.NodeID) {
		t.Fatalf("status identity = %#v", status.Identity)
	}
	if status.UpgradeProtocol != upgradeprotocol.Current || status.MinimumUpgradeProtocol != upgradeprotocol.Current {
		t.Fatalf("status upgrade protocol = %d/%d", status.UpgradeProtocol, status.MinimumUpgradeProtocol)
	}
}

func TestAgentClientStatusPreservesMembershipAttestation(t *testing.T) {
	identity, err := nodeidentity.New("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "node-1", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	membership := nodeidentity.InventoryNode{NodeID: identity.NodeID, NodeName: identity.NodeName, Lifecycle: nodeidentity.NodeLifecycleCordoned, Schedulable: false, AllocationPublicKey: identity.AllocationPublicKey}
	statusBody, _ := json.Marshal(AgentStatus{Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &identity.Identity, Membership: &membership, MembershipGeneration: 17})
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) { return http.StatusOK, nil, string(statusBody) }}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Membership == nil || status.Membership.NodeID != identity.NodeID || status.Membership.Lifecycle != nodeidentity.NodeLifecycleCordoned || status.MembershipGeneration != 17 {
		t.Fatalf("membership attestation was lost: %#v", status)
	}
}

func TestAgentClientStatusRejectsMalformedIdentity(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, `{"runtime":"takod","capabilities":["node.identity-v1"],"identity":{"apiVersion":"tako.io/v1","kind":"InstallationIdentity","clusterId":"not-a-uuid","nodeId":"also-bad","nodeName":"node-1","createdAt":"2026-07-16T12:00:00Z"}}`
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid installation identity") {
		t.Fatalf("Status error = %v", err)
	}
}

func TestAgentClientStatusRejectsMalformedUpgradeProtocolRange(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, `{"runtime":"takod","version":"v0.9.4","upgradeProtocol":1,"minimumUpgradeProtocol":2}`
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid node upgrade protocol") {
		t.Fatalf("malformed upgrade range error = %v", err)
	}
}

func TestAgentClientStatusRejectsUnknownIdentityFields(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, `{"runtime":"takod","capabilities":["node.identity-v1"],"identity":{"apiVersion":"tako.io/v1","kind":"InstallationIdentity","clusterId":"11111111-1111-4111-8111-111111111111","nodeId":"22222222-2222-4222-8222-222222222222","nodeName":"node-1","createdAt":"2026-07-16T12:00:00Z","forged":true}}`
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Status error = %v, want unknown identity field rejection", err)
	}
}

func TestAgentClientStatusRejectsIdentityWithoutCapability(t *testing.T) {
	identity, err := nodeidentity.New(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"node-1",
		[]string{nodeidentity.RoleWorker},
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(AgentStatus{Runtime: "takod", Identity: &identity.Identity})
	if err != nil {
		t.Fatal(err)
	}
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, string(body)
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "without node.identity-v1 capability") {
		t.Fatalf("Status error = %v", err)
	}
}

func TestAgentClientStatusBoundsUntrustedResponse(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		body    string
	}{
		{name: "body", body: strings.Repeat("x", int(agentStatusBodyLimit)+1)},
		{name: "headers", headers: http.Header{"X-Oversized": []string{strings.Repeat("x", agentMaxResponseHeaders+1)}}, body: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
				return http.StatusOK, test.headers, test.body
			}}
			client, err := NewAgentClient(dialer, DefaultSocket)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Status(context.Background()); err == nil {
				t.Fatal("Status should reject oversized untrusted response")
			}
		})
	}
}

func TestAgentClientStatusBoundsConcurrentProbes(t *testing.T) {
	entered := make(chan struct{}, maxConcurrentStatusProbes)
	release := make(chan struct{})
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		entered <- struct{}{}
		<-release
		return http.StatusOK, nil, `{"runtime":"takod"}`
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for index := 0; index < maxConcurrentStatusProbes; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = client.Status(context.Background())
		}()
	}
	for index := 0; index < maxConcurrentStatusProbes; index++ {
		<-entered
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Status(ctx); err == nil || !strings.Contains(err.Error(), "probe unavailable") {
		t.Fatalf("extra Status error = %v, want bounded probe rejection", err)
	}
	close(release)
	wait.Wait()
}

func TestAgentClientPreservesHTTPError(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusConflict, nil, "lease held"
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	output, err := client.RequestJSON(context.Background(), http.MethodPost, "/v1/lease", nil)
	if output != "lease held" {
		t.Fatalf("output = %q", output)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
		t.Fatalf("error = %#v, want HTTPError 409", err)
	}
}

func TestAgentClientDoesNotFollowRedirects(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusTemporaryRedirect, http.Header{"Location": []string{"http://attacker.invalid/"}}, "redirect denied"
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RequestJSON(context.Background(), http.MethodGet, "/v1/status", nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusTemporaryRedirect {
		t.Fatalf("error = %#v, want HTTPError 307", err)
	}
	if got := len(dialer.Paths()); got != 1 {
		t.Fatalf("redirect caused %d dials, want 1", got)
	}
}

func TestAgentClientStreamsResponseWithoutBuffering(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(request *http.Request) (int, http.Header, string) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if string(body) != "input" {
			t.Errorf("input body = %q", body)
		}
		return http.StatusOK, nil, "first\nsecond\n"
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := client.StreamOutput(context.Background(), http.MethodPost, "/v1/stream", strings.NewReader("input"), "application/octet-stream", &output, io.Discard); err != nil {
		t.Fatalf("StreamOutput returned error: %v", err)
	}
	if output.String() != "first\nsecond\n" {
		t.Fatalf("stream output = %q", output.String())
	}
}

func TestAgentClientRejectsInvalidEndpointBeforeDial(t *testing.T) {
	dialer := &pipeUnixDialer{handler: func(*http.Request) (int, http.Header, string) {
		return http.StatusOK, nil, "ok"
	}}
	client, err := NewAgentClient(dialer, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestJSON(context.Background(), http.MethodGet, "//attacker/path", nil); err == nil {
		t.Fatal("RequestJSON should reject a scheme-relative endpoint")
	}
	if len(dialer.Paths()) != 0 {
		t.Fatal("invalid endpoint should not be dialed")
	}
}
