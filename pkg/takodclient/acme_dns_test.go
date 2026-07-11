package takodclient

import (
	"errors"
	"testing"
	"time"
)

func TestParseACMEDNSErrorPreservesCooldownContract(t *testing.T) {
	retry := time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)
	err := ParseACMEDNSError("node-a", &HTTPError{
		Method: "PUT", Endpoint: "/v1/acme-dns", Status: 429,
		Body: `{"code":"cooldown","domain":"*.example.com","retryAfter":"2026-07-11T13:00:00Z","error":"wait before retrying"}`,
	})
	var operationErr *ACMEOperationError
	if !errors.As(err, &operationErr) || operationErr.Server != "node-a" || operationErr.Code != "cooldown" || operationErr.Domain != "*.example.com" || !operationErr.RetryAfter.Equal(retry) {
		t.Fatalf("error = %#v", err)
	}
}

func TestACMEDNSEndpointEscapesIdentity(t *testing.T) {
	if got := ACMEDNSEndpoint("demo", "production"); got != "/v1/acme-dns?environment=production&project=demo" {
		t.Fatalf("endpoint = %q", got)
	}
}
