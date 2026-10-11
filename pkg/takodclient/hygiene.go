package takodclient

// Reclaim-before-refusing (add-disk-hygiene-defaults D6). When a node that
// advertises node.disk-hygiene-v1 denies a request with HTTP 507 for
// free-disk admission (a body starting with "operation denied"), the client
// asks the node to reclaim space and retries the original request exactly
// once. Every such 507 is an admission check at handler entry, before any
// mutation, so a JSON request is safe to replay. A streamed body cannot be
// replayed: the client reports what was reclaimed and fails with the
// original error. Old nodes are never asked to reclaim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// CapabilityNodeDiskHygieneV1 mirrors takod.CapabilityNodeDiskHygieneV1.
	CapabilityNodeDiskHygieneV1 = "node.disk-hygiene-v1"
	// HygieneEndpoint is GET /v1/hygiene.
	HygieneEndpoint = "/v1/hygiene"
	// HygieneReclaimEndpoint is POST /v1/hygiene/reclaim.
	HygieneReclaimEndpoint = "/v1/hygiene/reclaim"
	// HygieneReclaimTimeout bounds the reclaim call: the node bounds the
	// reclaim at 5 minutes.
	HygieneReclaimTimeout   = 5*time.Minute + 30*time.Second
	httpInsufficientStorage = 507
)

// ReclaimEvent describes one reclaim the client requested after a denial.
type ReclaimEvent struct {
	// Server is the node's SSH host when the client exposes one.
	Server     string
	Endpoint   string
	BytesFreed int64
	// Retrying is true when the original request is retried; false when
	// its body could not be replayed.
	Retrying bool
}

var reclaimObserver atomic.Pointer[func(context.Context, ReclaimEvent)]

// SetReclaimObserver installs the process-wide reclaim observer (the CLI
// prints one line and emits a hygiene.reclaimed event). nil removes it.
func SetReclaimObserver(observer func(context.Context, ReclaimEvent)) {
	if observer == nil {
		reclaimObserver.Store(nil)
		return
	}
	reclaimObserver.Store(&observer)
}

func notifyReclaim(ctx context.Context, event ReclaimEvent) {
	if observer := reclaimObserver.Load(); observer != nil {
		(*observer)(ctx, event)
	}
}

type reclaimDisabledKey struct{}

// WithoutReclaim returns a context whose requests never trigger a reclaim.
func WithoutReclaim(ctx context.Context) context.Context {
	return context.WithValue(ctx, reclaimDisabledKey{}, true)
}

func reclaimDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(reclaimDisabledKey{}).(bool)
	return disabled
}

// IsDiskAdmissionDenial reports whether err is a takod free-disk admission
// denial: HTTP 507 with a body starting with "operation denied". The
// PostgreSQL verification memory check uses a different message and is not
// one.
func IsDiskAdmissionDenial(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	return isDiskAdmissionDenialResponse(httpErr.Status, httpErr.Body)
}

func isDiskAdmissionDenialResponse(status int, body string) bool {
	return status == httpInsufficientStorage && strings.HasPrefix(strings.TrimSpace(body), "operation denied")
}

type hostNamer interface{ Host() string }

func clientServerName(client any) string {
	if named, ok := client.(hostNamer); ok {
		return named.Host()
	}
	return ""
}

// reclaimAfterDenial asks a capable node to reclaim space. ok is false for
// nodes without the capability or when the reclaim call fails; the caller
// then fails exactly as before.
func reclaimAfterDenial(ctx context.Context, client any, socket string) (int64, bool) {
	ctx = WithoutReclaim(ctx)
	statusOutput, err := requestJSONOnce(ctx, client, socket, "GET", "/v1/status", nil, JSONRequestTimeout)
	if err != nil {
		return 0, false
	}
	var status struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal([]byte(statusOutput), &status) != nil {
		return 0, false
	}
	capable := false
	for _, capability := range status.Capabilities {
		if capability == CapabilityNodeDiskHygieneV1 {
			capable = true
			break
		}
	}
	if !capable {
		return 0, false
	}
	output, err := requestJSONOnce(ctx, client, socket, "POST", HygieneReclaimEndpoint, map[string]any{}, HygieneReclaimTimeout)
	if err != nil {
		return 0, false
	}
	var run struct {
		BytesFreed int64 `json:"bytesFreed"`
	}
	if json.Unmarshal([]byte(output), &run) != nil {
		return 0, false
	}
	return run.BytesFreed, true
}

// requestJSONWithReclaim runs one JSON request and, on a free-disk denial
// from a capable node, reclaims and retries it once. When the retry is
// denied again, the original error is returned.
func requestJSONWithReclaim(ctx context.Context, client any, socket, method, endpoint string, value any, timeout time.Duration) (string, error) {
	output, err := requestJSONOnce(ctx, client, socket, method, endpoint, value, timeout)
	if err == nil || reclaimDisabled(ctx) || strings.HasPrefix(endpoint, "/v1/hygiene") || !IsDiskAdmissionDenial(err) {
		return output, err
	}
	freed, ok := reclaimAfterDenial(ctx, client, socket)
	if !ok {
		return output, err
	}
	notifyReclaim(ctx, ReclaimEvent{Server: clientServerName(client), Endpoint: endpoint, BytesFreed: freed, Retrying: true})
	retryOutput, retryErr := requestJSONOnce(ctx, client, socket, method, endpoint, value, timeout)
	if retryErr != nil && IsDiskAdmissionDenial(retryErr) {
		return output, err
	}
	return retryOutput, retryErr
}

// ReclaimedNotRetriedError wraps the original denial of a request whose body
// could not be replayed after a reclaim.
type ReclaimedNotRetriedError struct {
	Err        error
	Server     string
	BytesFreed int64
}

func (e *ReclaimedNotRetriedError) Error() string {
	server := e.Server
	if server == "" {
		server = "the server"
	}
	return fmt.Sprintf("%v; disk was low on %s: reclaimed %s, re-run the command", e.Err, server, FormatBytes(e.BytesFreed))
}

func (e *ReclaimedNotRetriedError) Unwrap() error { return e.Err }

// reclaimAfterStreamDenial handles a denied streamed request: it reclaims
// on a capable node and returns the original error annotated, never
// retrying the consumed body.
func reclaimAfterStreamDenial(ctx context.Context, client any, socket, endpoint string, err error) error {
	if err == nil || reclaimDisabled(ctx) || !IsDiskAdmissionDenial(err) {
		return err
	}
	freed, ok := reclaimAfterDenial(ctx, client, socket)
	if !ok {
		return err
	}
	server := clientServerName(client)
	notifyReclaim(ctx, ReclaimEvent{Server: server, Endpoint: endpoint, BytesFreed: freed, Retrying: false})
	return &ReclaimedNotRetriedError{Err: err, Server: server, BytesFreed: freed}
}

// FormatBytes renders a byte count with a binary unit (e.g. "3.2 GB").
func FormatBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(value)/float64(div), "KMGTPE"[exp])
}
