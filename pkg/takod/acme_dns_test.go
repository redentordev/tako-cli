package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/v3/acme"
)

func TestReconcileACMEDNSIssuesStoresAndReusesWithoutSecretsInResult(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	secret := "cloudflare-zone-token-secret"
	calls := installFakeACMEDNSObtain(t, func(certificate ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	req := testACMEDNSRequest("*.example.com", secret)
	response, err := ReconcileACMEDNS(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(response.Certificates) != 1 || response.Certificates[0].Action != "issued" {
		t.Fatalf("calls=%d response=%+v", *calls, response)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "PRIVATE KEY") {
		t.Fatalf("result leaked secret material: %s", encoded)
	}
	metadata := response.Certificates[0].Certificate
	if metadata.Source != CertificateSourceACMEDNS || metadata.OwnerProject != "platform" || metadata.OwnerEnvironment != "production" || metadata.DNSProvider != ACMEDNSProviderCloudflare || metadata.Orphaned {
		t.Fatalf("metadata = %+v", metadata)
	}
	assertPrivateFile(t, acmeDNSCredentialsPath("platform", "production"))
	assertPrivateFile(t, acmeDNSOwnerPath("platform", "production"))

	response, err = ReconcileACMEDNS(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || response.Certificates[0].Action != "reused" {
		t.Fatalf("valid stored cert was not reused: calls=%d response=%+v", *calls, response)
	}
}

func TestReconcileACMEDNSRotatesCredentialWithoutReissuingOrRestartingProxy(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	calls := installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	request := testACMEDNSRequest("*.example.com", "old-zone-token")
	if _, err := ReconcileACMEDNS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	caddyBefore, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	request.Credentials["apiToken"] = "new-zone-token"
	response, err := ReconcileACMEDNS(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || response.Certificates[0].Action != "reused" {
		t.Fatalf("rotation caused issuance: calls=%d response=%+v", *calls, response)
	}
	credentialBody, err := os.ReadFile(acmeDNSCredentialsPath("platform", "production"))
	if err != nil || !strings.Contains(string(credentialBody), "new-zone-token") || strings.Contains(string(credentialBody), "old-zone-token") {
		t.Fatalf("rotated credential was not atomically replaced: err=%v body=%s", err, credentialBody)
	}
	caddyAfter, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || !bytes.Equal(caddyBefore, caddyAfter) {
		t.Fatalf("credential rotation changed proxy configuration: err=%v", err)
	}
}

func TestReconcileACMEDNSDoesNotReuseCertificateAcrossCASettings(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"app.example.com"})
	calls := installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: true}, nil
	})
	request := testACMEDNSRequest("app.example.com", "token")
	if _, err := ReconcileACMEDNS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Certificates[0].Staging = true
	response, err := ReconcileACMEDNS(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || response.Certificates[0].Action != "issued" || !response.Certificates[0].Certificate.Staging {
		t.Fatalf("CA setting change reused old certificate: calls=%d response=%+v", *calls, response)
	}
}

func TestPrepareACMEDNSFailureKeepsActiveOwnershipAndCredentials(t *testing.T) {
	useTempProxyPaths(t)
	oldCert, oldKey := testProxyCertificatePair(t, []string{"old.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(oldCert), PrivateKeyPEM: []byte(oldKey)}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("old.example.com", "working-token")); err != nil {
		t.Fatal(err)
	}
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{}, errors.New("new domain issuance failed")
	})
	if _, err := PrepareACMEDNS(context.Background(), testACMEDNSRequest("new.example.com", "replacement-token")); err == nil {
		t.Fatal("PrepareACMEDNS unexpectedly succeeded")
	}
	owners, err := loadActiveACMEDNSOwnerDocuments()
	if err != nil || len(owners) != 1 || len(owners[0].Certificates) != 1 || owners[0].Certificates[0].Domain != "old.example.com" {
		t.Fatalf("active owners=%+v err=%v", owners, err)
	}
	credentialBody, err := os.ReadFile(acmeDNSCredentialsPath("platform", "production"))
	if err != nil || !strings.Contains(string(credentialBody), "working-token") || strings.Contains(string(credentialBody), "replacement-token") {
		t.Fatalf("active credentials changed: err=%v body=%s", err, credentialBody)
	}
	entry, err := loadExactProxyCertificate("old.example.com", true)
	if err != nil || entry.Metadata.Orphaned {
		t.Fatalf("old certificate lost renewal eligibility: entry=%+v err=%v", entry, err)
	}
	if _, err := os.Stat(acmeDNSPendingPath("platform", "production")); !os.IsNotExist(err) {
		t.Fatalf("failed prepare left an ownership claim: %v", err)
	}
}

func TestRoutePublicationFailureDoesNotFinalizeStagedACMEDNSOwnership(t *testing.T) {
	useTempProxyPaths(t)
	oldCert, oldKey := testProxyCertificatePair(t, []string{"old.example.com"})
	newCert, newKey := testProxyCertificatePair(t, []string{"new.example.com"})
	installFakeACMEDNSObtain(t, func(certificate ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		if certificate.Domain == "old.example.com" {
			return acmeDNSCertificateMaterial{CertificatePEM: []byte(oldCert), PrivateKeyPEM: []byte(oldKey)}, nil
		}
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(newCert), PrivateKeyPEM: []byte(newKey)}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("old.example.com", "working-token")); err != nil {
		t.Fatal(err)
	}
	oldManifest := `{"version":1,"project":"platform","environment":"production","routes":[{"service":"web","domains":["old.example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "platform-production.json", Content: oldManifest}); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareACMEDNS(context.Background(), testACMEDNSRequest("new.example.com", "replacement-token")); err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	oldCaddyfilePath := proxyCaddyfilePath
	proxyCaddyfilePath = filepath.Join(blockedParent, "Caddyfile")
	t.Cleanup(func() { proxyCaddyfilePath = oldCaddyfilePath })
	newManifest := strings.ReplaceAll(oldManifest, "old.example.com", "new.example.com")
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "platform-production.json", Content: newManifest}); err == nil {
		t.Fatal("route publication unexpectedly succeeded")
	}
	owners, err := loadActiveACMEDNSOwnerDocuments()
	if err != nil || len(owners) != 1 || owners[0].Certificates[0].Domain != "old.example.com" {
		t.Fatalf("active owners=%+v err=%v", owners, err)
	}
}

func TestRecreateProxyWithACMEDNSCertificatePublishesSafeTLSConfig(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "")

	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "node-token")); err != nil {
		t.Fatal(err)
	}
	content := `{"version":1,"project":"consumer","environment":"production","routes":[{"service":"web","domains":["app.example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "consumer.json", Content: content}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: "tako_consumer_production"}); err != nil {
		t.Fatal(err)
	}
	caddyfile, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || !strings.Contains(string(caddyfile), "tls "+filepath.Join(proxyCertContainerDir, proxyCertVersionsDir)) {
		t.Fatalf("recreated proxy omitted managed wildcard: err=%v\n%s", err, caddyfile)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, proxyCertStoreDir+":"+proxyCertContainerDir+":ro") || strings.Contains(commands, "node-token") {
		t.Fatalf("unsafe recreated proxy command:\n%s", commands)
	}
}

func TestReconcileACMEDNSFailureRedactsAndEnforcesCooldown(t *testing.T) {
	useTempProxyPaths(t)
	secret := "bad-provider-token"
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	oldNow := acmeDNSNow
	acmeDNSNow = func() time.Time { return now }
	t.Cleanup(func() { acmeDNSNow = oldNow })
	calls := installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{}, errors.New("provider rejected token " + secret)
	})
	req := testACMEDNSRequest("*.example.com", secret)
	_, err := ReconcileACMEDNS(context.Background(), req)
	var operationErr *ACMEDNSError
	if err == nil || !errors.As(err, &operationErr) || operationErr.Code != "issuance_failed" || strings.Contains(err.Error(), secret) {
		t.Fatalf("first error = %#v", err)
	}
	listed, listErr := ListProxyCertificates(context.Background())
	if listErr != nil || len(listed.Certificates) != 1 || listed.Certificates[0].Domain != "*.example.com" || listed.Certificates[0].LastError == "" || listed.Certificates[0].RetryAfter == nil || strings.Contains(listed.Certificates[0].LastError, secret) {
		t.Fatalf("pollable failure list=%+v err=%v", listed, listErr)
	}
	failureBytes, readErr := os.ReadFile(acmeDNSFailurePath("*.example.com"))
	if readErr != nil || strings.Contains(string(failureBytes), secret) {
		t.Fatalf("failure state leaked secret: err=%v body=%s", readErr, failureBytes)
	}
	_, err = ReconcileACMEDNS(context.Background(), req)
	if err == nil || !errors.As(err, &operationErr) || operationErr.Code != "cooldown" || !operationErr.RetryAfter.Equal(now.Add(acmeDNSFailureCooldown)) {
		t.Fatalf("cooldown error = %#v", err)
	}
	if *calls != 1 {
		t.Fatalf("issuer calls = %d, want one", *calls)
	}
	if _, statErr := os.Stat(acmeDNSPendingPath("platform", "production")); !os.IsNotExist(statErr) {
		t.Fatalf("failed issuance left an ownership claim: %v", statErr)
	}
	for _, path := range []string{acmeDNSCredentialsPath("platform", "production"), acmeDNSOwnerPath("platform", "production")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("failed issuance changed active ACME state at %s: %v", path, statErr)
		}
	}
}

func TestReconcileACMEDNSClassifiesRateLimitRetryAfter(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	retry := now.Add(2 * time.Hour)
	oldNow := acmeDNSNow
	acmeDNSNow = func() time.Time { return now }
	t.Cleanup(func() { acmeDNSNow = oldNow })
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{}, acme.Problem{Status: 429, Type: "urn:ietf:params:acme:error:rateLimited", Detail: "retry after " + retry.Format(time.RFC3339)}
	})
	_, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("app.example.com", "token"))
	var operationErr *ACMEDNSError
	if err == nil || !errors.As(err, &operationErr) || operationErr.Code != "rate_limited" || !operationErr.RetryAfter.Equal(retry) {
		t.Fatalf("rate limit error = %#v", err)
	}
}

func TestRateLimitWithoutUsableRetryAfterUsesConservativeBackoff(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	failure := classifyACMEDNSFailure("platform", "production", "app.example.com", now, acme.Problem{
		Status: 429, Type: "urn:ietf:params:acme:error:rateLimited", Detail: "too many certificates already issued",
	})
	if failure.Code != "rate_limited" || !failure.RetryAfter.Equal(now.Add(acmeDNSRateLimitBackoff)) {
		t.Fatalf("failure = %+v", failure)
	}
	if got := parseRetryAfter("retry-after: 7200", now); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("delta retry-after = %s", got)
	}
	httpDate := now.Add(3 * time.Hour).Format(http.TimeFormat)
	if got := parseRetryAfter("retry after "+httpDate, now); !got.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("HTTP-date retry-after = %s", got)
	}
}

func TestRemoveACMEDNSConfigurationMarksOwnedCertificateOrphaned(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	response, err := RemoveACMEDNSConfiguration(context.Background(), "platform", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Orphaned) != 1 || response.Orphaned[0] != "*.example.com" {
		t.Fatalf("remove response = %+v", response)
	}
	entry, err := loadExactProxyCertificate("*.example.com", true)
	if err != nil || !entry.Metadata.Orphaned {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	for _, path := range []string{acmeDNSCredentialsPath("platform", "production"), acmeDNSOwnerPath("platform", "production")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("configuration still exists at %s: %v", path, err)
		}
	}
}

func TestRemoveManagedProxyCertificateRetiresOwnershipAndCredentials(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(acmeDNSFailurePath("*.example.com"), acmeDNSFailureDocument{Domain: "*.example.com"}); err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(acmeDNSStoragePath(), "certificates", "issuer", certmagic.StorageKeys.Safe("*.example.com"))
	if err := os.MkdirAll(managedPath, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wildcard_example.com.crt", "wildcard_example.com.key", "wildcard_example.com.json"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("managed"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := RemoveProxyCertificate(context.Background(), "*.example.com"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(proxyCertStoreDir, "*.example.com"),
		acmeDNSCredentialsPath("platform", "production"),
		acmeDNSOwnerPath("platform", "production"),
		acmeDNSFailurePath("*.example.com"),
		managedPath,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("managed certificate state still exists at %s: %v", path, err)
		}
	}
	if owners, err := loadACMEDNSOwnerDocuments(); err != nil || len(owners) != 0 {
		t.Fatalf("owners=%+v err=%v", owners, err)
	}
}

func TestReconcileACMEDNSRejectsCrossProjectOwnershipConflict(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	request := testACMEDNSRequest("*.example.com", "other-token")
	request.Project = "consumer"
	_, err := ReconcileACMEDNS(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "owned by platform/production") {
		t.Fatalf("ownership error = %v", err)
	}
}

func TestProxyRouteManifestAcceptsWildcardHostOnly(t *testing.T) {
	manifest := `{"version":1,"project":"platform","environment":"production","routes":[{"service":"web","domains":["*.example.com"],"upstreams":["http://platform-web:3000"]}]}`
	if _, err := ParseProxyRouteManifest(manifest); err != nil {
		t.Fatalf("wildcard route rejected: %v", err)
	}
	bad := strings.Replace(manifest, "*.example.com", "foo.*.example.com", 1)
	if _, err := ParseProxyRouteManifest(bad); err == nil {
		t.Fatal("embedded wildcard route was accepted")
	}
}

func TestCaddyRenderRejectsConsumerCoveredByUnissuedOwnerCertificate(t *testing.T) {
	manifest := ProxyRouteManifest{
		Version: 1, Project: "consumer", Environment: "production",
		Routes: []ProxyRoute{{Service: "web", Domains: []string{"app.example.com"}, Upstreams: []string{"http://consumer-web:3000"}}},
	}
	owners := []acmeDNSOwnerClaim{{Domain: "*.example.com", Project: "platform", Environment: "production"}}
	_, err := renderCaddyfileWithCertificatesAndOwners([]ProxyRouteManifest{manifest}, nil, owners)
	if err == nil || !strings.Contains(err.Error(), "platform/production") || !strings.Contains(err.Error(), "deploy the owning configuration first") {
		t.Fatalf("render error = %v", err)
	}
	if owner := selectACMEDNSOwner(owners, "deep.app.example.com"); owner != nil {
		t.Fatalf("single-label wildcard covered deep hostname: %+v", owner)
	}
}

func TestProxyACMEDNSEndpointReturnsTypedRedactedFailure(t *testing.T) {
	useTempProxyPaths(t)
	secret := "endpoint-secret-token"
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{}, errors.New("provider echoed " + secret)
	})
	body, _ := json.Marshal(testACMEDNSRequest("app.example.com", secret))
	req := httptest.NewRequest(http.MethodPut, "/v1/acme-dns", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	NewServer("/tmp/unused.sock", t.TempDir(), "test").handleProxyACMEDNS(recorder, req)
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), secret) || !strings.Contains(recorder.Body.String(), `"code":"issuance_failed"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestProxyACMEDNSEndpointFinalizesOnlyAfterExplicitPost(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"app.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	body, _ := json.Marshal(testACMEDNSRequest("app.example.com", "token"))
	server := NewServer("/tmp/unused.sock", t.TempDir(), "test")
	recorder := httptest.NewRecorder()
	server.handleProxyACMEDNS(recorder, httptest.NewRequest(http.MethodPut, "/v1/acme-dns", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("prepare status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertPrivateFile(t, acmeDNSPendingPath("platform", "production"))
	if _, err := os.Stat(acmeDNSOwnerPath("platform", "production")); !os.IsNotExist(err) {
		t.Fatalf("prepare activated ownership: %v", err)
	}
	recorder = httptest.NewRecorder()
	server.handleProxyACMEDNS(recorder, httptest.NewRequest(http.MethodPost, "/v1/acme-dns?project=platform&environment=production", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("premature finalize status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	manifest := `{"version":1,"project":"platform","environment":"production","routes":[{"service":"web","domains":["app.example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "platform-production.json", Content: manifest}); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	server.handleProxyACMEDNS(recorder, httptest.NewRequest(http.MethodPost, "/v1/acme-dns?project=platform&environment=production", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("finalize status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertPrivateFile(t, acmeDNSOwnerPath("platform", "production"))
	assertPrivateFile(t, acmeDNSCredentialsPath("platform", "production"))
	if _, err := os.Stat(acmeDNSPendingPath("platform", "production")); !os.IsNotExist(err) {
		t.Fatalf("finalize left staged state: %v", err)
	}
}

func testACMEDNSRequest(domain, token string) ACMEDNSReconcileRequest {
	return ACMEDNSReconcileRequest{
		Project: "platform", Environment: "production", DNSProvider: ACMEDNSProviderCloudflare,
		Credentials:  map[string]string{"apiToken": token},
		Certificates: []ACMEDNSCertificateRequest{{Domain: domain, Email: "ops@example.com", CAProvider: ACMEDNSCAProviderLetsEncrypt}},
	}
}

func installFakeACMEDNSObtain(t *testing.T, issue func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error)) *int {
	t.Helper()
	old := acmeDNSObtain
	calls := 0
	acmeDNSObtain = func(_ context.Context, _ string, _ map[string]string, certificate ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		calls++
		return issue(certificate)
	}
	t.Cleanup(func() { acmeDNSObtain = old })
	return &calls
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("%s mode = %v", path, info.Mode())
	}
	root := filepath.Clean(proxyCertStoreDir)
	if rel, err := filepath.Rel(root, path); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("private file escaped store: %s", path)
	}
}
