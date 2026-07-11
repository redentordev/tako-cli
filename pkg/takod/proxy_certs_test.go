package takod

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertificateRollbackFailureKeepsActiveVersionIntact(t *testing.T) {
	useTempProxyPaths(t)
	certOne, keyOne := testProxyCertificatePair(t, []string{"example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certOne, KeyPEM: keyOne}); err != nil {
		t.Fatal(err)
	}
	oldRender := proxyCertificateRender
	oldPublishLink := proxyCertificatePublishLink
	proxyCertificateRender = func(context.Context) error { return errors.New("caddy publish failed") }
	linkCalls := 0
	proxyCertificatePublishLink = func(target string, relativeVersion string) error {
		linkCalls++
		if linkCalls == 2 {
			return errors.New("rollback link failed")
		}
		return oldPublishLink(target, relativeVersion)
	}
	t.Cleanup(func() {
		proxyCertificateRender = oldRender
		proxyCertificatePublishLink = oldPublishLink
	})
	certTwo, keyTwo := testProxyCertificatePair(t, []string{"example.com"})
	_, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certTwo, KeyPEM: keyTwo})
	if err == nil || !strings.Contains(err.Error(), "certificate rollback failed") {
		t.Fatalf("PushProxyCertificate error = %v", err)
	}
	target := filepath.Join(proxyCertStoreDir, "example.com")
	linkTarget, linkErr := proxyCertificateLinkTarget(target)
	if linkErr != nil {
		t.Fatalf("active link became dangling after rollback failure: %v", linkErr)
	}
	if _, statErr := os.Stat(filepath.Join(proxyCertStoreDir, linkTarget, proxyCertificateFile)); statErr != nil {
		t.Fatalf("active version was deleted after rollback failure: %v", statErr)
	}
}

func TestCertificateStoreRejectsSymlinkedDirectoriesAndSecuresModes(t *testing.T) {
	t.Run("store symlink", func(t *testing.T) {
		useTempProxyPaths(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, proxyCertStoreDir); err != nil {
			t.Fatal(err)
		}
		if _, err := ListProxyCertificates(context.Background()); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("ListProxyCertificates error = %v", err)
		}
	})
	t.Run("versions symlink", func(t *testing.T) {
		useTempProxyPaths(t)
		if err := os.MkdirAll(proxyCertStoreDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(proxyCertStoreDir, proxyCertVersionsDir)); err != nil {
			t.Fatal(err)
		}
		certPEM, keyPEM := testProxyCertificatePair(t, []string{"example.com"})
		if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certPEM, KeyPEM: keyPEM}); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("PushProxyCertificate error = %v", err)
		}
	})
	t.Run("mode correction", func(t *testing.T) {
		useTempProxyPaths(t)
		if err := os.MkdirAll(proxyCertStoreDir, 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(proxyCertStoreDir, 0777); err != nil {
			t.Fatal(err)
		}
		if _, err := ListProxyCertificates(context.Background()); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(proxyCertStoreDir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("store mode = %v err=%v", info.Mode().Perm(), err)
		}
	})
}

func TestProxyCertificatesEndpointNeverReturnsPrivateKey(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"example.com"})
	body, err := json.Marshal(ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certPEM, KeyPEM: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/v1/certs", strings.NewReader(string(body)))
	new(Server).handleProxyCertificates(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "PRIVATE KEY") || strings.Contains(recorder.Body.String(), "keyPem") {
		t.Fatalf("PUT response leaked private material: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/certs", nil)
	new(Server).handleProxyCertificates(recorder, request)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "PRIVATE KEY") {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPushListAndRemoveProxyCertificate(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"example.com"})
	response, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "Example.COM.", CertPEM: certPEM, KeyPEM: keyPEM})
	if err != nil {
		t.Fatalf("PushProxyCertificate returned error: %v", err)
	}
	if response.Certificate.Domain != "example.com" || response.Certificate.Source != CertificateSourcePushed {
		t.Fatalf("certificate = %#v", response.Certificate)
	}
	linkInfo, err := os.Lstat(filepath.Join(proxyCertStoreDir, "example.com"))
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("active certificate entry is not an atomic symlink: info=%v err=%v", linkInfo, err)
	}
	linkTarget, err := os.Readlink(filepath.Join(proxyCertStoreDir, "example.com"))
	if err != nil || !strings.HasPrefix(linkTarget, proxyCertVersionsDir+string(filepath.Separator)) {
		t.Fatalf("active certificate link = %q err=%v", linkTarget, err)
	}
	for _, name := range []string{proxyCertificateFile, proxyPrivateKeyFile, proxyCertMetadataFile} {
		info, err := os.Stat(filepath.Join(proxyCertStoreDir, "example.com", name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
	list, err := ListProxyCertificates(context.Background())
	if err != nil {
		t.Fatalf("ListProxyCertificates returned error: %v", err)
	}
	if len(list.Certificates) != 1 || list.Certificates[0].Domain != "example.com" || list.Certificates[0].NotAfter.IsZero() {
		t.Fatalf("certificates = %#v", list.Certificates)
	}
	if _, err := RemoveProxyCertificate(context.Background(), "example.com"); err != nil {
		t.Fatalf("RemoveProxyCertificate returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proxyCertStoreDir, "example.com")); !os.IsNotExist(err) {
		t.Fatalf("certificate directory still exists: %v", err)
	}
}

func TestReplacingCertificateChangesCaddyLoadPath(t *testing.T) {
	useTempProxyPaths(t)
	content := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "demo.json", Content: content}); err != nil {
		t.Fatal(err)
	}
	certOne, keyOne := testProxyCertificatePair(t, []string{"example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certOne, KeyPEM: keyOne}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	certTwo, keyTwo := testProxyCertificatePair(t, []string{"example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certTwo, KeyPEM: keyTwo}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) || !strings.Contains(string(second), "/.versions/") {
		t.Fatalf("certificate replacement did not force a new Caddy load path:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestRemovingCertificateRepublishesExistingRouteForAutomaticHTTPS(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"*.example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "*.example.com", CertPEM: certPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatal(err)
	}
	content := `{"version":1,"project":"customer","environment":"production","routes":[{"service":"web","domains":["app.example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "customer.json", Content: content}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || !strings.Contains(string(before), "\ttls ") {
		t.Fatalf("store-backed route missing tls before remove: err=%v\n%s", err, before)
	}
	if _, err := RemoveProxyCertificate(context.Background(), "*.example.com"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(caddySiteBlock(t, string(after), "app.example.com"), "\ttls ") {
		t.Fatalf("route retained explicit TLS after certificate removal:\n%s", after)
	}
}

func TestExpiredStoredCertificateCanStillBeListedAndRemoved(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatal(err)
	}
	oldNow := proxyCertificateNow
	proxyCertificateNow = func() time.Time { return time.Now().Add(48 * time.Hour) }
	t.Cleanup(func() { proxyCertificateNow = oldNow })
	list, err := ListProxyCertificates(context.Background())
	if err != nil || len(list.Certificates) != 1 {
		t.Fatalf("ListProxyCertificates certificates=%#v err=%v", list, err)
	}
	if _, err := RemoveProxyCertificate(context.Background(), "example.com"); err != nil {
		t.Fatalf("RemoveProxyCertificate expired entry: %v", err)
	}
}

func TestPushProxyCertificateRejectsGarbageAndMismatchWithoutChangingCaddyfile(t *testing.T) {
	useTempProxyPaths(t)
	if err := os.MkdirAll(filepath.Dir(proxyCaddyfilePath), 0755); err != nil {
		t.Fatal(err)
	}
	const previous = "existing-caddyfile\n"
	if err := os.WriteFile(proxyCaddyfilePath, []byte(previous), 0644); err != nil {
		t.Fatal(err)
	}
	validCert, _ := testProxyCertificatePair(t, []string{"example.com"})
	_, otherKey := testProxyCertificatePair(t, []string{"example.com"})
	wrongDomainCert, wrongDomainKey := testProxyCertificatePair(t, []string{"other.example.com"})
	expiredCert, expiredKey := testProxyCertificatePairWithValidity(t, []string{"example.com"}, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	for _, req := range []ProxyCertificatePushRequest{
		{Domain: "example.com", CertPEM: "garbage", KeyPEM: "garbage"},
		{Domain: "example.com", CertPEM: validCert, KeyPEM: otherKey},
		{Domain: "example.com", CertPEM: wrongDomainCert, KeyPEM: wrongDomainKey},
		{Domain: "example.com", CertPEM: expiredCert, KeyPEM: expiredKey},
	} {
		if _, err := PushProxyCertificate(context.Background(), req); err == nil {
			t.Fatalf("PushProxyCertificate(%q) returned nil", req.CertPEM)
		}
		data, err := os.ReadFile(proxyCaddyfilePath)
		if err != nil || string(data) != previous {
			t.Fatalf("Caddyfile changed after rejected pair: data=%q err=%v", data, err)
		}
	}
}

func TestProxyCertificateSelectionExactWildcardAutomaticAcrossProjects(t *testing.T) {
	useTempProxyPaths(t)
	wildcardCert, wildcardKey := testProxyCertificatePair(t, []string{"*.platform.example.com"})
	exactCert, exactKey := testProxyCertificatePair(t, []string{"exact.platform.example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "*.platform.example.com", CertPEM: wildcardCert, KeyPEM: wildcardKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "exact.platform.example.com", CertPEM: exactCert, KeyPEM: exactKey}); err != nil {
		t.Fatal(err)
	}
	manifests := []ProxyRouteManifest{
		{Version: 1, Project: "ingress", Environment: "production", Routes: []ProxyRoute{{Service: "owner", Domains: []string{"owner.platform.example.com"}, Upstreams: []string{"http://owner:3000"}}}},
		{Version: 1, Project: "customer", Environment: "production", Routes: []ProxyRoute{{Service: "apps", Domains: []string{"app.platform.example.com", "exact.platform.example.com", "automatic.example.net"}, Upstreams: []string{"http://apps:3000"}}}},
	}
	entries, err := loadProxyCertificateEntries(true)
	if err != nil {
		t.Fatal(err)
	}
	caddyfile, err := renderCaddyfileWithCertificates(manifests, entries)
	if err != nil {
		t.Fatal(err)
	}
	var wildcardPath, exactPath string
	for _, entry := range entries {
		switch entry.Metadata.Domain {
		case "*.platform.example.com":
			wildcardPath = entry.CertPath
		case "exact.platform.example.com":
			exactPath = entry.CertPath
		}
	}
	appBlock := caddySiteBlock(t, caddyfile, "app.platform.example.com")
	if !strings.Contains(appBlock, "tls "+wildcardPath) {
		t.Fatalf("covered cross-project route did not use wildcard:\n%s", appBlock)
	}
	exactBlock := caddySiteBlock(t, caddyfile, "exact.platform.example.com")
	if !strings.Contains(exactBlock, "tls "+exactPath) || strings.Contains(exactBlock, wildcardPath) {
		t.Fatalf("exact certificate did not beat wildcard:\n%s", exactBlock)
	}
	automaticBlock := caddySiteBlock(t, caddyfile, "automatic.example.net")
	if strings.Contains(automaticBlock, "\ttls ") {
		t.Fatalf("uncovered route did not retain automatic HTTPS:\n%s", automaticBlock)
	}
	if _, err := RemoveProxyCertificate(context.Background(), "*.platform.example.com"); err != nil {
		t.Fatalf("RemoveProxyCertificate wildcard: %v", err)
	}
	remaining, err := loadProxyCertificateEntries(true)
	if err != nil {
		t.Fatal(err)
	}
	afterRemove, err := renderCaddyfileWithCertificates(manifests, remaining)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(caddySiteBlock(t, afterRemove, "app.platform.example.com"), "\ttls ") {
		t.Fatalf("wildcard removal did not return covered route to automatic HTTPS:\n%s", afterRemove)
	}
	if !strings.Contains(caddySiteBlock(t, afterRemove, "exact.platform.example.com"), "tls ") {
		t.Fatalf("wildcard removal disturbed exact certificate:\n%s", afterRemove)
	}
}

func TestStoredCertificateIsRevalidatedBeforeCaddyPublication(t *testing.T) {
	useTempProxyPaths(t)
	certPEM, keyPEM := testProxyCertificatePair(t, []string{"example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxyCertStoreDir, "example.com", proxyPrivateKeyFile), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderAndWriteCaddyfile(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid stored certificate") {
		t.Fatalf("renderAndWriteCaddyfile error = %v", err)
	}
}

func TestRecreateProxyWithStoredCertificatePublishesSafeTLSConfig(t *testing.T) {
	useTempProxyPaths(t)
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "")

	certPEM, keyPEM := testProxyCertificatePair(t, []string{"example.com"})
	if _, err := PushProxyCertificate(context.Background(), ProxyCertificatePushRequest{Domain: "example.com", CertPEM: certPEM, KeyPEM: keyPEM}); err != nil {
		t.Fatal(err)
	}
	content := `{"version":1,"project":"demo","environment":"production","routes":[{"service":"web","domains":["example.com"],"upstreams":["http://web:3000"]}]}`
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "demo.json", Content: content}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileProxy(context.Background(), ReconcileProxyRequest{Network: "tako_demo_production"}); err != nil {
		t.Fatalf("ReconcileProxy returned error: %v", err)
	}
	caddyfile, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(caddyfile), "tls "+filepath.Join(proxyCertContainerDir, proxyCertVersionsDir)) {
		t.Fatalf("recreated proxy Caddyfile missing stored certificate:\n%s", caddyfile)
	}
	commands := strings.Join(readCommandLog(t, logPath), "\n")
	if !strings.Contains(commands, proxyCertStoreDir+":"+proxyCertContainerDir+":ro") {
		t.Fatalf("recreated proxy did not mount certificate store read-only:\n%s", commands)
	}
}

func caddySiteBlock(t *testing.T, caddyfile string, domain string) string {
	t.Helper()
	start := strings.Index(caddyfile, "\n"+domain+" {")
	if start < 0 {
		t.Fatalf("missing %s block:\n%s", domain, caddyfile)
	}
	block := caddyfile[start+1:]
	end := strings.Index(block, "\n}")
	if end < 0 {
		t.Fatalf("unterminated %s block", domain)
	}
	return block[:end+2]
}

func testProxyCertificatePair(t *testing.T, dnsNames []string) (string, string) {
	t.Helper()
	now := time.Now().UTC()
	return testProxyCertificatePairWithValidity(t, dnsNames, now.Add(-time.Hour), now.Add(24*time.Hour))
}

func testProxyCertificatePairWithValidity(t *testing.T, dnsNames []string, notBefore time.Time, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(certPEM), string(keyPEM)
}
