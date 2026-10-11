package takod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

func writeCaddyStoredCertificate(t *testing.T, issuer string, name string, certPEM string) {
	t.Helper()
	safe := certmagic.StorageKeys.Safe(name)
	dir := filepath.Join(proxyCaddyDataDir, "caddy", "certificates", issuer, safe)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, safe+".crt"), []byte(certPEM), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProxyCertificateStatusReadsCaddyManagedCertificate(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Now().UTC()
	expired, _ := testProxyCertificatePairWithValidity(t, []string{"origin.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	writeCaddyStoredCertificate(t, "acme-staging-v02.api.letsencrypt.org-directory", "origin.example.com", expired)
	valid, _ := testProxyCertificatePair(t, []string{"origin.example.com"})
	writeCaddyStoredCertificate(t, "acme-v02.api.letsencrypt.org-directory", "origin.example.com", valid)

	status, err := ProxyCertificateStatus(context.Background(), "Origin.Example.com.")
	if err != nil {
		t.Fatal(err)
	}
	if status.Domain != "origin.example.com" || !status.Present || !status.Valid || status.Source != CertificateSourceCaddy || status.NotAfter.Before(now) {
		t.Fatalf("status = %#v, want valid caddy certificate", status)
	}
}

func TestProxyCertificateStatusReportsExpiredAndMissing(t *testing.T) {
	useTempProxyPaths(t)
	missing, err := ProxyCertificateStatus(context.Background(), "origin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Present || missing.Valid {
		t.Fatalf("missing status = %#v", missing)
	}

	now := time.Now().UTC()
	expired, _ := testProxyCertificatePairWithValidity(t, []string{"origin.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	writeCaddyStoredCertificate(t, "acme-v02.api.letsencrypt.org-directory", "origin.example.com", expired)
	status, err := ProxyCertificateStatus(context.Background(), "origin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Present || status.Valid || !strings.Contains(status.Error, "expired") {
		t.Fatalf("expired status = %#v", status)
	}
}

func TestProxyCertificateStatusUsesCaddyWildcardAndStorePrecedence(t *testing.T) {
	useTempProxyPaths(t)
	wildcard, _ := testProxyCertificatePair(t, []string{"*.example.com"})
	writeCaddyStoredCertificate(t, "acme-v02.api.letsencrypt.org-directory", "*.example.com", wildcard)
	status, err := ProxyCertificateStatus(context.Background(), "origin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Valid || status.Name != "*.example.com" || status.Source != CertificateSourceCaddy {
		t.Fatalf("wildcard status = %#v", status)
	}

	oldRender := proxyCertificateRender
	proxyCertificateRender = func(context.Context) error { return nil }
	t.Cleanup(func() { proxyCertificateRender = oldRender })
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"origin.example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "origin.example.com", CertPEM: certPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatal(err)
	}
	status, err = ProxyCertificateStatus(context.Background(), "origin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Valid || status.Source != CertificateSourcePushed || status.Name != "origin.example.com" {
		t.Fatalf("store status = %#v, want pushed certificate precedence", status)
	}
}

func TestProxyCertificateStatusEndpointIsReadOnlyAndKeyFree(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, _ := testProxyCertificatePair(t, []string{"origin.example.com"})
	writeCaddyStoredCertificate(t, "acme-v02.api.letsencrypt.org-directory", "origin.example.com", certPEM)

	recorder := httptest.NewRecorder()
	new(Server).handleProxyCertificateStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/certs/status?domain=origin.example.com", nil))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "BEGIN") {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response ProxyCertificateStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || !response.Valid {
		t.Fatalf("response = %#v err=%v", response, err)
	}

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/certs/status?domain=origin.example.com", nil),
		httptest.NewRequest(http.MethodGet, "/v1/certs/status?domain=*.example.com", nil),
		httptest.NewRequest(http.MethodGet, "/v1/certs/status?domain=../etc", nil),
	} {
		recorder = httptest.NewRecorder()
		new(Server).handleProxyCertificateStatus(recorder, request)
		if recorder.Code == http.StatusOK {
			t.Fatalf("%s %s unexpectedly succeeded", request.Method, request.URL)
		}
	}
}
