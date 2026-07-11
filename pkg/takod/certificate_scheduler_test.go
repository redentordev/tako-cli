package takod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertificateSchedulerRenewsReferencedCertificateAndEmitsEvent(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	oldNow := acmeDNSNow
	acmeDNSNow = func() time.Time { return now }
	t.Cleanup(func() { acmeDNSNow = oldNow })
	initialCert, initialKey := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-60*24*time.Hour), now.Add(20*24*time.Hour))
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(initialCert), PrivateKeyPEM: []byte(initialKey), Changed: true}, nil
	})
	request := testACMEDNSRequest("*.example.com", "rotation-safe-token")
	if _, err := ReconcileACMEDNS(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	writeACMEOwnerRouteManifest(t, "*.example.com")

	renewedCert, renewedKey := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	acmeDNSObtain = func(context.Context, string, map[string]string, ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(renewedCert), PrivateKeyPEM: []byte(renewedKey), Changed: true}, nil
	}
	dataDir := t.TempDir()
	if err := NewCertificateScheduler(dataDir).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry, err := loadExactProxyCertificate("*.example.com", true)
	if err != nil || !entry.Metadata.NotAfter.Equal(now.Add(90*24*time.Hour)) || entry.Metadata.LastSuccessAt == nil {
		t.Fatalf("renewed entry=%+v err=%v", entry, err)
	}
	eventsPath := filepath.Join(dataDir, "events", "platform", "production.jsonl")
	eventBytes, err := os.ReadFile(eventsPath)
	if err != nil || !strings.Contains(string(eventBytes), "cert.renew.completed") || strings.Contains(string(eventBytes), "rotation-safe-token") {
		t.Fatalf("renewal events err=%v body=%s", err, eventBytes)
	}
}

func TestCertificateSchedulerChecksHealthyForARIAndSkipsOrphanedCertificates(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	oldNow := acmeDNSNow
	acmeDNSNow = func() time.Time { return now }
	t.Cleanup(func() { acmeDNSNow = oldNow })
	certPEM, keyPEM := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	calls := installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: true}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	writeACMEOwnerRouteManifest(t, "*.example.com")
	before := *calls
	acmeDNSObtain = func(context.Context, string, map[string]string, ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		(*calls)++
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: false}, nil
	}
	if err := NewCertificateScheduler(t.TempDir()).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != before+1 {
		t.Fatalf("healthy cert did not enter CertMagic ARI check: calls=%d before=%d", *calls, before)
	}
	afterHealthyCheck := *calls
	if _, err := RemoveACMEDNSConfiguration(context.Background(), "platform", "production"); err != nil {
		t.Fatal(err)
	}
	if err := NewCertificateScheduler(t.TempDir()).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != afterHealthyCheck {
		t.Fatalf("orphaned cert triggered renewal: calls=%d before=%d", *calls, afterHealthyCheck)
	}
}

func TestCertificateSchedulerFailureIsPollableAndRedacted(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	oldNow := acmeDNSNow
	acmeDNSNow = func() time.Time { return now }
	t.Cleanup(func() { acmeDNSNow = oldNow })
	certPEM, keyPEM := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-60*24*time.Hour), now.Add(20*24*time.Hour))
	secret := "renewal-token-secret"
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: true}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", secret)); err != nil {
		t.Fatal(err)
	}
	writeACMEOwnerRouteManifest(t, "*.example.com")
	acmeDNSObtain = func(context.Context, string, map[string]string, ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{}, &testACMEProviderError{message: "bad token " + secret}
	}
	dataDir := t.TempDir()
	if err := NewCertificateScheduler(dataDir).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry, err := loadExactProxyCertificate("*.example.com", true)
	if err != nil || entry.Metadata.LastAttemptAt == nil || entry.Metadata.LastError == "" || entry.Metadata.RetryAfter == nil || strings.Contains(entry.Metadata.LastError, secret) {
		t.Fatalf("failure metadata=%+v err=%v", entry, err)
	}
	eventBytes, err := os.ReadFile(filepath.Join(dataDir, "events", "platform", "production.jsonl"))
	if err != nil || !strings.Contains(string(eventBytes), "cert.renew.failed") || strings.Contains(string(eventBytes), secret) {
		t.Fatalf("failure event err=%v body=%s", err, eventBytes)
	}
}

func TestCertificateSchedulerMarksOwnerWithoutCurrentRouteOrphaned(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	oldNow := acmeDNSNow
	acmeDNSNow = func() time.Time { return now }
	t.Cleanup(func() { acmeDNSNow = oldNow })
	certPEM, keyPEM := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	calls := installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: true}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	owners, err := loadACMEDNSOwnerDocuments()
	if err != nil || len(owners) != 1 {
		t.Fatalf("owners=%+v err=%v", owners, err)
	}
	owners[0].UpdatedAt = now.Add(-20 * time.Minute)
	if err := writePrivateJSON(acmeDNSOwnerPath("platform", "production"), owners[0]); err != nil {
		t.Fatal(err)
	}
	before := *calls
	if err := NewCertificateScheduler(t.TempDir()).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry, err := loadExactProxyCertificate("*.example.com", true)
	if err != nil || !entry.Metadata.Orphaned {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	if *calls != before {
		t.Fatalf("unreferenced owner renewed: calls=%d before=%d", *calls, before)
	}
}

func TestCertificateSchedulerDoesNotResurrectExplicitlyRemovedCertificate(t *testing.T) {
	useTempProxyPaths(t)
	now := time.Now().UTC()
	initialCert, initialKey := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(20*24*time.Hour))
	renewedCert, renewedKey := testProxyCertificatePairWithValidity(t, []string{"*.example.com"}, now.Add(-time.Minute), now.Add(90*24*time.Hour))
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(initialCert), PrivateKeyPEM: []byte(initialKey), Changed: true}, nil
	})
	if _, err := ReconcileACMEDNS(context.Background(), testACMEDNSRequest("*.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	writeACMEOwnerRouteManifest(t, "*.example.com")
	entered := make(chan struct{})
	release := make(chan struct{})
	acmeDNSObtain = func(context.Context, string, map[string]string, ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		close(entered)
		<-release
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(renewedCert), PrivateKeyPEM: []byte(renewedKey), Changed: true}, nil
	}
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- NewCertificateScheduler(t.TempDir()).Check(context.Background()) }()
	<-entered
	if err := os.Remove(filepath.Join(proxyRoutesDir, "platform-production.json")); err != nil {
		t.Fatal(err)
	}
	removeDone := make(chan error, 1)
	go func() {
		if _, err := RemoveACMEDNSConfiguration(context.Background(), "platform", "production"); err != nil {
			removeDone <- err
			return
		}
		_, err := RemoveProxyCertificate(context.Background(), "*.example.com")
		removeDone <- err
	}()
	close(release)
	if err := <-schedulerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(proxyCertStoreDir, "*.example.com")); !os.IsNotExist(err) {
		t.Fatalf("removed certificate was resurrected: %v", err)
	}
}

func TestCertificateSchedulerContinuesAfterOwnerCredentialFailure(t *testing.T) {
	useTempProxyPaths(t)
	if err := os.MkdirAll(proxyRoutesDir, 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, tc := range []struct {
		project string
		domain  string
	}{
		{project: "alpha", domain: "alpha.example.com"},
		{project: "bravo", domain: "bravo.example.net"},
	} {
		certPEM, keyPEM := testProxyCertificatePairWithValidity(t, []string{tc.domain}, now.Add(-time.Hour), now.Add(20*24*time.Hour))
		installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
			return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: true}, nil
		})
		request := testACMEDNSRequest(tc.domain, tc.project+"-token")
		request.Project = tc.project
		if _, err := ReconcileACMEDNS(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		manifest := ProxyRouteManifest{Version: 1, Project: tc.project, Environment: "production", Routes: []ProxyRoute{{Service: "web", Domains: []string{tc.domain}, Upstreams: []string{"http://web:3000"}}}}
		data, _ := json.Marshal(manifest)
		if err := writeFileAtomic(filepath.Join(proxyRoutesDir, tc.project+"-production.json"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(acmeDNSCredentialsPath("alpha", "production"), 0644); err != nil {
		t.Fatal(err)
	}
	checkedBravo := 0
	acmeDNSObtain = func(_ context.Context, _ string, credentials map[string]string, _ ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		if credentials["apiToken"] == "bravo-token" {
			checkedBravo++
		}
		return acmeDNSCertificateMaterial{Changed: false}, nil
	}
	dataDir := t.TempDir()
	if err := NewCertificateScheduler(dataDir).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checkedBravo != 1 {
		t.Fatalf("later owner checks = %d, want 1", checkedBravo)
	}
	alpha, err := loadExactProxyCertificate("alpha.example.com", false)
	if err != nil || alpha.Metadata.LastError == "" {
		t.Fatalf("alpha failure was not pollable: entry=%+v err=%v", alpha, err)
	}
	eventBody, err := os.ReadFile(filepath.Join(dataDir, "events", "alpha", "production.jsonl"))
	if err != nil || !strings.Contains(string(eventBody), "credential_unavailable") {
		t.Fatalf("alpha credential event missing: err=%v body=%s", err, eventBody)
	}
}

func TestCertificateSchedulerRecoversPublishedPendingOwnershipAfterCrash(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"app.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM), Changed: false}, nil
	})
	if _, err := PrepareACMEDNS(context.Background(), testACMEDNSRequest("app.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proxyRoutesDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := ProxyRouteManifest{Version: 1, Project: "platform", Environment: "production", Routes: []ProxyRoute{{Service: "web", Domains: []string{"app.example.com"}, Upstreams: []string{"http://web:3000"}}}}
	data, _ := json.Marshal(manifest)
	if err := writeFileAtomic(filepath.Join(proxyRoutesDir, "platform-production.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := NewCertificateScheduler(t.TempDir()).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	owners, err := loadActiveACMEDNSOwnerDocuments()
	if err != nil || len(owners) != 1 || owners[0].Certificates[0].Domain != "app.example.com" {
		t.Fatalf("recovered owners=%+v err=%v", owners, err)
	}
	if _, err := os.Stat(acmeDNSPendingPath("platform", "production")); !os.IsNotExist(err) {
		t.Fatalf("recovered pending state still exists: %v", err)
	}
}

func TestCertificateSchedulerExpiresAbandonedSuccessfulPrepare(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"app.example.com"})
	installFakeACMEDNSObtain(t, func(ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
		return acmeDNSCertificateMaterial{CertificatePEM: []byte(certPEM), PrivateKeyPEM: []byte(keyPEM)}, nil
	})
	if _, err := PrepareACMEDNS(context.Background(), testACMEDNSRequest("app.example.com", "token")); err != nil {
		t.Fatal(err)
	}
	pending, err := readACMEDNSPending("platform", "production")
	if err != nil {
		t.Fatal(err)
	}
	pending.Owner.UpdatedAt = acmeDNSNow().UTC().Add(-acmeDNSPendingTTL - time.Minute)
	if err := writePrivateJSON(acmeDNSPendingPath("platform", "production"), pending); err != nil {
		t.Fatal(err)
	}
	if err := NewCertificateScheduler(t.TempDir()).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acmeDNSPendingPath("platform", "production")); !os.IsNotExist(err) {
		t.Fatalf("abandoned pending state still exists: %v", err)
	}
	entry, err := loadExactProxyCertificate("app.example.com", true)
	if err != nil || !entry.Metadata.Orphaned {
		t.Fatalf("abandoned certificate was not orphaned: entry=%+v err=%v", entry, err)
	}
}

type testACMEProviderError struct{ message string }

func (e *testACMEProviderError) Error() string { return e.message }

func writeACMEOwnerRouteManifest(t *testing.T, domain string) {
	t.Helper()
	if err := os.MkdirAll(proxyRoutesDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := ProxyRouteManifest{
		Version: 1, Project: "platform", Environment: "production",
		Routes: []ProxyRoute{{Service: "web", Domains: []string{domain}, Upstreams: []string{"http://platform-web:3000"}}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(proxyRoutesDir, "platform-production.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}
