package takodclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type hygieneNode struct {
	mu          sync.Mutex
	capable     bool
	denials     []string
	requests    []string
	reclaimBody string
}

func (n *hygieneNode) handler(t *testing.T) func(*http.Request) (int, http.Header, string) {
	return func(request *http.Request) (int, http.Header, string) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.requests = append(n.requests, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case "/v1/status":
			capabilities := `["node.identity-v1"]`
			if n.capable {
				capabilities = `["node.identity-v1","node.disk-hygiene-v1"]`
			}
			return http.StatusOK, nil, `{"runtime":"takod","capabilities":` + capabilities + `}`
		case HygieneReclaimEndpoint:
			body := n.reclaimBody
			if body == "" {
				body = `{"trigger":"request","bytesFreed":3435973837}`
			}
			return http.StatusOK, nil, body
		default:
			if len(n.denials) > 0 {
				body := n.denials[0]
				n.denials = n.denials[1:]
				return 507, nil, body
			}
			return http.StatusOK, nil, `{"ok":true}`
		}
	}
}

func (n *hygieneNode) calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.requests...)
}

func captureReclaims(t *testing.T) *[]ReclaimEvent {
	t.Helper()
	var mu sync.Mutex
	events := &[]ReclaimEvent{}
	SetReclaimObserver(func(_ context.Context, event ReclaimEvent) {
		mu.Lock()
		defer mu.Unlock()
		*events = append(*events, event)
	})
	t.Cleanup(func() { SetReclaimObserver(nil) })
	return events
}

func TestRequestJSONReclaimsAndRetriesOnceAfterDiskDenial(t *testing.T) {
	events := captureReclaims(t)
	node := &hygieneNode{capable: true, denials: []string{"operation denied on /var/lib/docker: 1 free bytes is below 2-byte platform minimum\n"}}
	dialer := &pipeUnixDialer{handler: node.handler(t)}
	output, err := RequestJSONWithTimeoutContext(context.Background(), dialer, DefaultSocket, "PUT", "/v1/service-files", map[string]string{"project": "shop"}, 0)
	if err != nil || output != `{"ok":true}` {
		t.Fatalf("retry = %q, %v", output, err)
	}
	want := []string{"PUT /v1/service-files", "GET /v1/status", "POST /v1/hygiene/reclaim", "PUT /v1/service-files"}
	if got := node.calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(*events) != 1 || (*events)[0].BytesFreed != 3435973837 || !(*events)[0].Retrying || (*events)[0].Endpoint != "/v1/service-files" {
		t.Fatalf("events = %+v", *events)
	}
}

func TestRequestJSONSecondDenialReturnsOriginalError(t *testing.T) {
	captureReclaims(t)
	node := &hygieneNode{capable: true, denials: []string{"operation denied first", "operation denied second"}}
	dialer := &pipeUnixDialer{handler: node.handler(t)}
	_, err := RequestJSONWithTimeoutContext(context.Background(), dialer, DefaultSocket, "POST", "/v1/reconcile-service", map[string]string{}, 0)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 507 || !strings.Contains(httpErr.Body, "first") {
		t.Fatalf("error = %v, want the original 507", err)
	}
	if len(node.calls()) != 4 {
		t.Fatalf("calls = %v, want exactly one retry", node.calls())
	}
}

func TestRequestJSONOldNodeIsNotAskedToReclaim(t *testing.T) {
	events := captureReclaims(t)
	node := &hygieneNode{capable: false, denials: []string{"operation denied on /var/lib/tako"}}
	dialer := &pipeUnixDialer{handler: node.handler(t)}
	_, err := RequestJSONWithTimeoutContext(context.Background(), dialer, DefaultSocket, "POST", "/v1/reconcile-service", map[string]string{}, 0)
	if !IsDiskAdmissionDenial(err) {
		t.Fatalf("error = %v", err)
	}
	for _, call := range node.calls() {
		if strings.Contains(call, "/v1/hygiene") {
			t.Fatalf("old node received a reclaim call: %v", node.calls())
		}
	}
	if len(*events) != 0 {
		t.Fatalf("events = %+v", *events)
	}
}

func TestRequestJSONDoesNotRetryMemoryDenial(t *testing.T) {
	captureReclaims(t)
	node := &hygieneNode{capable: true, denials: []string{"insufficient available memory for isolated PostgreSQL verification"}}
	dialer := &pipeUnixDialer{handler: node.handler(t)}
	_, err := RequestJSONWithTimeoutContext(context.Background(), dialer, DefaultSocket, "POST", "/v1/backups/postgres/verify", map[string]string{}, 0)
	if err == nil || IsDiskAdmissionDenial(err) {
		t.Fatalf("error = %v", err)
	}
	if got := node.calls(); len(got) != 1 {
		t.Fatalf("memory denial must not reclaim or retry: %v", got)
	}
}

func TestStreamRequestDenialReclaimsWithoutRetry(t *testing.T) {
	events := captureReclaims(t)
	node := &hygieneNode{capable: true, denials: []string{"operation denied on /var/lib/docker"}}
	dialer := &pipeUnixDialer{handler: node.handler(t)}
	_, err := StreamRequestWithContext(context.Background(), dialer, DefaultSocket, "POST", "/v1/images/import", strings.NewReader("tar"))
	var reclaimed *ReclaimedNotRetriedError
	if !errors.As(err, &reclaimed) || !strings.Contains(err.Error(), "re-run the command") || !strings.Contains(err.Error(), "3.2 GB") {
		t.Fatalf("error = %v", err)
	}
	if !IsDiskAdmissionDenial(err) {
		t.Fatalf("the original denial must stay visible: %v", err)
	}
	imports := 0
	for _, call := range node.calls() {
		if call == "POST /v1/images/import" {
			imports++
		}
	}
	if imports != 1 || len(*events) != 1 || (*events)[0].Retrying {
		t.Fatalf("imports=%d events=%+v", imports, *events)
	}
}

func TestFormatBytes(t *testing.T) {
	for value, want := range map[int64]string{512: "512 B", 2048: "2.0 KB", 3435973837: "3.2 GB"} {
		if got := FormatBytes(value); got != want {
			t.Fatalf("FormatBytes(%d) = %q, want %q", value, got, want)
		}
	}
}
