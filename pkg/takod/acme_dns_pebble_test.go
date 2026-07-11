package takod

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
	"github.com/miekg/dns"
)

// TestPebbleACMEDNSIssueStoreRenderRenew exercises the real certmagic ACME
// client against Pebble. The fake provider only translates libdns writes into
// pebble-challtestsrv's management API; Pebble still performs DNS-01
// validation through its DNS server.
func TestPebbleACMEDNSIssueStoreRenderRenew(t *testing.T) {
	directory := os.Getenv("TAKO_PEBBLE_DIRECTORY")
	challengeAPI := os.Getenv("TAKO_PEBBLE_CHALLTESTSRV")
	resolver := os.Getenv("TAKO_PEBBLE_RESOLVER")
	rootPath := os.Getenv("TAKO_PEBBLE_ROOT")
	if directory == "" || challengeAPI == "" || resolver == "" || rootPath == "" {
		t.Skip("Pebble environment is not configured")
	}

	rootPEM, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	trustedRoots := x509.NewCertPool()
	if !trustedRoots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("failed to parse Pebble root certificate")
	}
	resolver = startPebbleResolver(t, resolver, "example.org.")

	useTempProxyPaths(t)
	oldFactory := acmeDNSProviderFactory
	oldRuntime := acmeDNSIssuerRuntime
	oldManageSync := acmeDNSManageSync
	manageCalls := 0
	acmeDNSManageSync = func(ctx context.Context, magic *certmagic.Config, domains []string) error {
		manageCalls++
		return oldManageSync(ctx, magic, domains)
	}
	acmeDNSProviderFactory = func(string, map[string]string) (certmagic.DNSProvider, error) {
		return &pebbleChallengeProvider{endpoint: challengeAPI, client: http.DefaultClient}, nil
	}
	forceRenew := false
	acmeDNSIssuerRuntime = func() acmeDNSIssuerRuntimeOptions {
		return acmeDNSIssuerRuntimeOptions{
			CAURL: directory, TrustedRoots: trustedRoots, Resolvers: []string{resolver},
			PropagationTimeout: 30 * time.Second, ForceRenew: forceRenew,
		}
	}
	t.Cleanup(func() {
		acmeDNSProviderFactory = oldFactory
		acmeDNSIssuerRuntime = oldRuntime
		acmeDNSManageSync = oldManageSync
	})

	request := testACMEDNSRequest("*.example.org", "pebble-fake-token")
	request.Certificates[0].Email = "pebble@example.org"
	issued, err := ReconcileACMEDNS(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(issued.Certificates) != 1 || issued.Certificates[0].Action != "issued" {
		t.Fatalf("issuance response = %+v", issued)
	}

	consumerManifest := ProxyRouteManifest{
		Version: 1, Project: "consumer", Environment: "production",
		Routes: []ProxyRoute{{Service: "web", Domains: []string{"app.example.org"}, Upstreams: []string{"http://consumer-web:3000"}}},
	}
	manifestJSON, err := json.Marshal(consumerManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteProxyFile(context.Background(), ProxyFileRequest{Name: "consumer-production.json", Content: string(manifestJSON)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || !strings.Contains(string(before), "tls ") || !strings.Contains(string(before), "/.versions/") {
		t.Fatalf("issued wildcard was not rendered: err=%v\n%s", err, before)
	}

	// The owner route is the renewal reference. Pebble certificates are short
	// lived. First prove the normal daily path enters CertMagic's synchronous
	// ManageSync ARI refresh without forcing a new order; then force renewal to
	// prove export and reload behavior.
	writeACMEOwnerRouteManifest(t, "*.example.org")
	dataDir := t.TempDir()
	if err := NewCertificateScheduler(dataDir).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manageCalls != 1 {
		t.Fatalf("foreground ARI management calls = %d, want 1", manageCalls)
	}
	ariChecked, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil || !bytes.Equal(before, ariChecked) {
		t.Fatalf("ARI check unexpectedly republished certificate: err=%v", err)
	}
	forceRenew = true
	if err := NewCertificateScheduler(dataDir).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(proxyCaddyfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("renewal did not publish a new certificate path for Caddy reload")
	}
	eventBody, err := os.ReadFile(filepath.Join(dataDir, "events", "platform", "production.jsonl"))
	if err != nil || !strings.Contains(string(eventBody), "cert.renew.completed") {
		t.Fatalf("renewal event missing: err=%v body=%s", err, eventBody)
	}
}

// pebble-challtestsrv intentionally implements a minimal authoritative DNS
// server and answers SOA queries with NOTIMP while still returning its
// synthetic SOA in the authority section. This test-local forwarder exposes
// that SOA as a successful answer and removes the unsupported EDNS option while
// preserving the real challenge server for discovery and propagation checks.
func startPebbleResolver(t *testing.T, upstream, zone string) string {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{
		PacketConn: packetConn,
		Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
			if len(request.Question) == 1 && request.Question[0].Qtype == dns.TypeSOA && dns.IsSubDomain(zone, request.Question[0].Name) {
				response := new(dns.Msg)
				response.SetReply(request)
				response.Authoritative = true
				response.Answer = []dns.RR{&dns.SOA{
					Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 1},
					Ns:  "ns." + zone, Mbox: "hostmaster." + zone, Serial: 1, Refresh: 1, Retry: 1, Expire: 1, Minttl: 1,
				}}
				_ = writer.WriteMsg(response)
				return
			}
			forward := request.Copy()
			forward.Extra = slices.DeleteFunc(forward.Extra, func(record dns.RR) bool {
				_, isOPT := record.(*dns.OPT)
				return isOPT
			})
			response, _, exchangeErr := new(dns.Client).ExchangeContext(context.Background(), forward, upstream)
			if exchangeErr != nil {
				response = new(dns.Msg)
				response.SetRcode(request, dns.RcodeServerFailure)
			}
			_ = writer.WriteMsg(response)
		}),
	}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	return packetConn.LocalAddr().String()
}

type pebbleChallengeProvider struct {
	endpoint string
	client   *http.Client
}

func (p *pebbleChallengeProvider) AppendRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	for _, record := range records {
		rr := record.RR()
		if rr.Type != "TXT" {
			return nil, fmt.Errorf("unexpected Pebble test record type %s", rr.Type)
		}
		if err := p.post(ctx, "/set-txt", map[string]string{"host": ensureTrailingDot(libdns.AbsoluteName(rr.Name, zone)), "value": rr.Data}); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (p *pebbleChallengeProvider) DeleteRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	for _, record := range records {
		rr := record.RR()
		if err := p.post(ctx, "/clear-txt", map[string]string{"host": ensureTrailingDot(libdns.AbsoluteName(rr.Name, zone))}); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (p *pebbleChallengeProvider) post(ctx context.Context, path string, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("pebble challenge API returned HTTP %d", response.StatusCode)
	}
	return nil
}

func ensureTrailingDot(name string) string {
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}
