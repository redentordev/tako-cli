package takodclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
)

const ACMEDNSRequestTimeout = 5 * time.Minute

// ACMEOperationError preserves takod's issuance classification so a control
// plane can distinguish local cooldown and CA rate-limit responses from other
// deployment failures.
type ACMEOperationError struct {
	Server     string
	Code       string
	Domain     string
	RetryAfter time.Time
	Completed  []ACMECompletedOperation
	Err        error
}

type ACMECompletedOperation struct {
	Domain      string                   `json:"domain"`
	Action      string                   `json:"action"`
	Certificate ACMECompletedCertificate `json:"certificate"`
}

type ACMECompletedCertificate struct {
	NotAfter time.Time `json:"notAfter"`
}

func (e *ACMEOperationError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("ACME DNS operation on %s failed (%s)", e.Server, e.Code)
}

func (e *ACMEOperationError) Unwrap() error { return e.Err }

func ACMEDNSEndpoint(project, environment string) string {
	values := url.Values{}
	if project != "" {
		values.Set("project", project)
	}
	if environment != "" {
		values.Set("environment", environment)
	}
	if encoded := values.Encode(); encoded != "" {
		return "/v1/acme-dns?" + encoded
	}
	return "/v1/acme-dns"
}

// ParseACMEDNSError converts a structured takod error response into a stable
// typed error. Non-ACME and malformed responses retain their original type.
func ParseACMEDNSError(server string, err error) error {
	if err == nil {
		return nil
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	var payload struct {
		Code       string                   `json:"code"`
		Domain     string                   `json:"domain"`
		RetryAfter time.Time                `json:"retryAfter"`
		Error      string                   `json:"error"`
		Completed  []ACMECompletedOperation `json:"completed"`
	}
	if json.Unmarshal([]byte(httpErr.Body), &payload) != nil || payload.Code == "" {
		return err
	}
	message := payload.Error
	if message == "" {
		message = httpErr.Error()
	}
	return &ACMEOperationError{
		Server: server, Code: payload.Code, Domain: payload.Domain,
		RetryAfter: payload.RetryAfter, Completed: payload.Completed, Err: errors.New(message),
	}
}
