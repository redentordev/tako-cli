package takod

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/caddyserver/zerossl"
	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	hetzner "github.com/libdns/hetzner/v2"
	"github.com/mholt/acmez/v3/acme"
	"go.uber.org/zap"
)

const (
	CapabilityAcmeDNSV1 = "proxy.acme-dns-v1"

	ACMEDNSProviderCloudflare   = "cloudflare"
	ACMEDNSProviderHetzner      = "hetzner"
	ACMEDNSProviderDigitalOcean = "digitalocean"

	ACMEDNSCAProviderLetsEncrypt = "letsencrypt"
	ACMEDNSCAProviderZeroSSL     = "zerossl"

	acmeDNSCredentialsDir        = "credentials"
	acmeDNSOwnersDir             = "owners"
	acmeDNSPendingDir            = "pending"
	acmeDNSFailuresDir           = "failures"
	takodACMEDNSOperationTimeout = 4*time.Minute + 30*time.Second
)

var (
	acmeDNSNow              = time.Now
	acmeDNSFailureCooldown  = 15 * time.Minute
	acmeDNSRateLimitBackoff = 24 * time.Hour
	acmeDNSPendingTTL       = time.Hour
	acmeDNSObtain           = obtainACMEDNSCertificate
	acmeDNSProviderFactory  = newACMEDNSProvider
	acmeDNSIssuerRuntime    = func() acmeDNSIssuerRuntimeOptions { return acmeDNSIssuerRuntimeOptions{} }
	acmeDNSManageSync       = func(ctx context.Context, magic *certmagic.Config, domains []string) error {
		return magic.ManageSync(ctx, domains)
	}
)

type acmeDNSIssuerRuntimeOptions struct {
	CAURL              string
	TrustedRoots       *x509.CertPool
	Resolvers          []string
	PropagationTimeout time.Duration
	ForceRenew         bool
}

type ACMEDNSCertificateRequest struct {
	Domain     string `json:"domain"`
	Email      string `json:"email,omitempty"`
	CAProvider string `json:"caProvider,omitempty"`
	Staging    bool   `json:"staging,omitempty"`
}

type ACMEDNSReconcileRequest struct {
	Project      string                      `json:"project"`
	Environment  string                      `json:"environment"`
	DNSProvider  string                      `json:"dnsProvider"`
	Credentials  map[string]string           `json:"credentials"`
	Certificates []ACMEDNSCertificateRequest `json:"certificates"`
}

type ACMEDNSCertificateResult struct {
	Domain      string                   `json:"domain"`
	Action      string                   `json:"action"`
	Certificate ProxyCertificateMetadata `json:"certificate"`
}

type ACMEDNSReconcileResponse struct {
	Certificates []ACMEDNSCertificateResult `json:"certificates"`
}

type ACMEDNSRemoveResponse struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Orphaned    []string `json:"orphaned"`
}

// ACMEDNSError is serialized by the takod API so callers can distinguish a
// local retry cooldown from a CA account rate limit and back off correctly.
type ACMEDNSError struct {
	Code       string                     `json:"code"`
	Domain     string                     `json:"domain,omitempty"`
	RetryAfter time.Time                  `json:"retryAfter,omitempty"`
	Completed  []ACMEDNSCertificateResult `json:"completed,omitempty"`
	Err        error                      `json:"-"`
}

func (e *ACMEDNSError) Error() string {
	if e == nil {
		return "ACME DNS operation failed"
	}
	message := "ACME DNS operation failed"
	if e.Err != nil {
		message = e.Err.Error()
	}
	if !e.RetryAfter.IsZero() {
		message += fmt.Sprintf("; retry after %s", e.RetryAfter.UTC().Format(time.RFC3339))
	}
	return message
}

func (e *ACMEDNSError) Unwrap() error { return e.Err }

type acmeDNSCredentialDocument struct {
	DNSProvider string            `json:"dnsProvider"`
	Credentials map[string]string `json:"credentials"`
}

type acmeDNSOwnerDocument struct {
	Project      string                      `json:"project"`
	Environment  string                      `json:"environment"`
	DNSProvider  string                      `json:"dnsProvider"`
	Certificates []ACMEDNSCertificateRequest `json:"certificates"`
	UpdatedAt    time.Time                   `json:"updatedAt"`
}

type acmeDNSPendingDocument struct {
	Owner       acmeDNSOwnerDocument      `json:"owner"`
	Credentials acmeDNSCredentialDocument `json:"credentials"`
}

type acmeDNSOwnerClaim struct {
	Domain      string
	Project     string
	Environment string
}

type acmeDNSFailureDocument struct {
	Domain      string    `json:"domain"`
	Code        string    `json:"code"`
	FailedAt    time.Time `json:"failedAt"`
	RetryAfter  time.Time `json:"retryAfter"`
	Error       string    `json:"error"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	DNSProvider string    `json:"dnsProvider,omitempty"`
	CAProvider  string    `json:"caProvider,omitempty"`
	Staging     bool      `json:"staging,omitempty"`
}

type acmeDNSCertificateMaterial struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Changed        bool
}

// ReconcileACMEDNS is the in-process atomic convenience used by tests and
// callers that do not publish a separate route manifest. The HTTP deploy flow
// uses PrepareACMEDNS followed by FinalizeACMEDNS after route publication.
func ReconcileACMEDNS(ctx context.Context, req ACMEDNSReconcileRequest) (*ACMEDNSReconcileResponse, error) {
	response, err := PrepareACMEDNS(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := finalizeACMEDNS(ctx, req.Project, req.Environment, false); err != nil {
		return nil, err
	}
	return response, nil
}

func PrepareACMEDNS(ctx context.Context, req ACMEDNSReconcileRequest) (*ACMEDNSReconcileResponse, error) {
	if err := validateACMEDNSReconcileRequest(&req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	if err := ensureACMEDNSDirectories(); err != nil {
		return nil, err
	}
	if err := validateACMEDNSOwnershipAvailable(req); err != nil {
		return nil, err
	}

	credentialDoc := acmeDNSCredentialDocument{DNSProvider: req.DNSProvider, Credentials: cloneStringMap(req.Credentials)}
	ownerDoc := acmeDNSOwnerDocument{
		Project: req.Project, Environment: req.Environment, DNSProvider: req.DNSProvider,
		Certificates: append([]ACMEDNSCertificateRequest(nil), req.Certificates...), UpdatedAt: acmeDNSNow().UTC(),
	}
	if err := writePrivateJSON(acmeDNSPendingPath(req.Project, req.Environment), acmeDNSPendingDocument{Owner: ownerDoc, Credentials: credentialDoc}); err != nil {
		return nil, fmt.Errorf("failed to stage ACME DNS configuration: %w", err)
	}

	response := &ACMEDNSReconcileResponse{Certificates: make([]ACMEDNSCertificateResult, 0, len(req.Certificates))}
	for _, certificate := range req.Certificates {
		result, err := reconcileOwnedACMEDNSCertificate(ctx, req, certificate)
		if err != nil {
			redacted := redactACMEDNSError(err, req.Credentials)
			var operationErr *ACMEDNSError
			if errors.As(redacted, &operationErr) {
				operationErr.Completed = append([]ACMEDNSCertificateResult(nil), response.Certificates...)
			}
			_ = os.Remove(acmeDNSPendingPath(req.Project, req.Environment))
			activeOwners, activeErr := loadActiveACMEDNSOwnerDocuments()
			if activeErr == nil {
				activeDomains := map[string]bool{}
				for _, owner := range activeOwners {
					if owner.Project == req.Project && owner.Environment == req.Environment {
						activeDomains = ownedDomainSet(owner.Certificates)
						break
					}
				}
				_, _ = markACMEDNSCertificatesOrphaned(req.Project, req.Environment, activeDomains)
			}
			return nil, redacted
		}
		response.Certificates = append(response.Certificates, result)
	}

	return response, nil
}

// FinalizeACMEDNS promotes a staged provider/ownership document only after the
// matching route manifest was published successfully. Until this point the
// previous active document and credentials remain the scheduler's source of
// truth, so a failed domain replacement cannot orphan a still-live route.
func FinalizeACMEDNS(ctx context.Context, project, environment string) (*ACMEDNSReconcileResponse, error) {
	return finalizeACMEDNS(ctx, project, environment, true)
}

func finalizeACMEDNS(ctx context.Context, project, environment string, requirePublishedRoutes bool) (*ACMEDNSReconcileResponse, error) {
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return nil, fmt.Errorf("invalid ACME DNS project/environment")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	pending, err := readACMEDNSPending(project, environment)
	if err != nil {
		return nil, err
	}
	if requirePublishedRoutes {
		manifests, err := readProxyRouteManifests(proxyRoutesDir)
		if err != nil {
			return nil, err
		}
		referenced := acmeDNSOwnerRouteReferences(pending.Owner, manifests)
		if len(referenced) != len(pending.Owner.Certificates) {
			return nil, fmt.Errorf("cannot finalize ACME DNS ownership before every staged certificate is referenced by the published %s/%s route manifest", project, environment)
		}
	}
	credentialsPath := acmeDNSCredentialsPath(project, environment)
	previousCredentials, hadCredentials, err := readFileIfExists(credentialsPath)
	if err != nil {
		return nil, err
	}
	if err := writePrivateJSON(credentialsPath, pending.Credentials); err != nil {
		return nil, fmt.Errorf("failed to promote ACME DNS credentials: %w", err)
	}
	pending.Owner.UpdatedAt = acmeDNSNow().UTC()
	if err := writePrivateJSON(acmeDNSOwnerPath(project, environment), pending.Owner); err != nil {
		if hadCredentials {
			_ = writeFileAtomic(credentialsPath, previousCredentials, 0600)
		} else {
			_ = os.Remove(credentialsPath)
		}
		return nil, fmt.Errorf("failed to promote ACME DNS ownership: %w", err)
	}
	if err := reconcileACMEDNSOrphans(project, environment, ownedDomainSet(pending.Owner.Certificates)); err != nil {
		return nil, err
	}
	if err := os.Remove(acmeDNSPendingPath(project, environment)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to clear staged ACME DNS configuration: %w", err)
	}
	return &ACMEDNSReconcileResponse{Certificates: []ACMEDNSCertificateResult{}}, nil
}

func validateACMEDNSOwnershipAvailable(req ACMEDNSReconcileRequest) error {
	owners, err := loadACMEDNSOwnerDocuments()
	if err != nil {
		return err
	}
	for _, certificate := range req.Certificates {
		for _, owner := range owners {
			if owner.Project == req.Project && owner.Environment == req.Environment {
				continue
			}
			for _, claimed := range owner.Certificates {
				if strings.EqualFold(claimed.Domain, certificate.Domain) || wildcardCoversHost(claimed.Domain, certificate.Domain) || wildcardCoversHost(certificate.Domain, claimed.Domain) {
					return fmt.Errorf("certificate %s overlaps ACME DNS certificate %s owned by %s/%s; covered consumer hostnames must not issue", certificate.Domain, claimed.Domain, owner.Project, owner.Environment)
				}
			}
		}
		entry, loadErr := loadExactProxyCertificate(certificate.Domain, false)
		if loadErr != nil {
			if os.IsNotExist(loadErr) {
				continue
			}
			return loadErr
		}
		if entry.Metadata.Source != CertificateSourceACMEDNS {
			return fmt.Errorf("certificate %s is pushed manually; remove it before enabling managed DNS-01", certificate.Domain)
		}
		if entry.Metadata.OwnerProject != req.Project || entry.Metadata.OwnerEnvironment != req.Environment {
			return fmt.Errorf("certificate %s is owned by %s/%s", certificate.Domain, entry.Metadata.OwnerProject, entry.Metadata.OwnerEnvironment)
		}
	}
	return nil
}

func RemoveACMEDNSConfiguration(ctx context.Context, project, environment string) (*ACMEDNSRemoveResponse, error) {
	if !isSafeProjectName(project) || !isSafeRuntimeName(environment) {
		return nil, fmt.Errorf("invalid ACME DNS project/environment")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	if err := ensureACMEDNSDirectories(); err != nil {
		return nil, err
	}
	for _, path := range []string{acmeDNSOwnerPath(project, environment), acmeDNSCredentialsPath(project, environment), acmeDNSPendingPath(project, environment)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to remove ACME DNS configuration: %w", err)
		}
	}
	failures, err := loadACMEDNSFailures()
	if err != nil {
		return nil, err
	}
	for _, failure := range failures {
		if failure.Project == project && failure.Environment == environment {
			if err := os.Remove(acmeDNSFailurePath(failure.Domain)); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to remove ACME DNS failure state: %w", err)
			}
		}
	}
	orphaned, err := markACMEDNSCertificatesOrphaned(project, environment, nil)
	if err != nil {
		return nil, err
	}
	return &ACMEDNSRemoveResponse{Project: project, Environment: environment, Orphaned: orphaned}, nil
}

func reconcileOwnedACMEDNSCertificate(ctx context.Context, req ACMEDNSReconcileRequest, certificate ACMEDNSCertificateRequest) (ACMEDNSCertificateResult, error) {
	now := acmeDNSNow().UTC()
	entry, err := loadExactProxyCertificate(certificate.Domain, true)
	if err == nil {
		if entry.Metadata.Source != CertificateSourceACMEDNS {
			return ACMEDNSCertificateResult{}, fmt.Errorf("certificate %s is pushed manually; remove it before enabling managed DNS-01", certificate.Domain)
		}
		if entry.Metadata.OwnerProject != req.Project || entry.Metadata.OwnerEnvironment != req.Environment {
			return ACMEDNSCertificateResult{}, fmt.Errorf("certificate %s is owned by %s/%s", certificate.Domain, entry.Metadata.OwnerProject, entry.Metadata.OwnerEnvironment)
		}
		storedCAProvider := entry.Metadata.CAProvider
		if storedCAProvider == "" {
			storedCAProvider = ACMEDNSCAProviderLetsEncrypt
		}
		if storedCAProvider == certificate.CAProvider && entry.Metadata.Staging == certificate.Staging {
			entry.Metadata.Orphaned = false
			entry.Metadata.DNSProvider = req.DNSProvider
			entry.Metadata.CAProvider = certificate.CAProvider
			entry.Metadata.UpdatedAt = now
			if err := updateProxyCertificateMetadata(certificate.Domain, entry.Metadata); err != nil {
				return ACMEDNSCertificateResult{}, err
			}
			return ACMEDNSCertificateResult{Domain: certificate.Domain, Action: "reused", Certificate: entry.Metadata}, nil
		}
	}
	if err != nil && !os.IsNotExist(err) && !strings.Contains(err.Error(), "expired at") {
		return ACMEDNSCertificateResult{}, err
	}

	if cooldown, err := readACMEDNSFailure(certificate.Domain); err != nil {
		return ACMEDNSCertificateResult{}, err
	} else if cooldown != nil && now.Before(cooldown.RetryAfter) {
		return ACMEDNSCertificateResult{}, &ACMEDNSError{
			Code: "cooldown", Domain: certificate.Domain, RetryAfter: cooldown.RetryAfter,
			Err: fmt.Errorf("certificate issuance for %s is in cooldown after a failed attempt", certificate.Domain),
		}
	}

	operationCtx, cancel := context.WithTimeout(ctx, takodACMEDNSOperationTimeout)
	material, err := acmeDNSObtain(operationCtx, req.DNSProvider, req.Credentials, certificate)
	cancel()
	if err != nil {
		failure := classifyACMEDNSFailure(req.Project, req.Environment, certificate.Domain, now, err)
		failure.DNSProvider = req.DNSProvider
		failure.CAProvider = certificate.CAProvider
		failure.Staging = certificate.Staging
		failure.Error = redactACMEText(failure.Error, req.Credentials)
		_ = writePrivateJSON(acmeDNSFailurePath(certificate.Domain), failure)
		_ = recordACMEDNSAttemptMetadata(certificate.Domain, now, nil, failure.Error, &failure.RetryAfter)
		return ACMEDNSCertificateResult{}, &ACMEDNSError{Code: failure.Code, Domain: certificate.Domain, RetryAfter: failure.RetryAfter, Err: err}
	}

	succeededAt := acmeDNSNow().UTC()
	validated, err := validateProxyCertificatePair(certificate.Domain, material.CertificatePEM, material.PrivateKeyPEM, CertificateSourceACMEDNS, succeededAt, true)
	if err != nil {
		return ACMEDNSCertificateResult{}, fmt.Errorf("ACME returned invalid certificate material for %s: %w", certificate.Domain, err)
	}
	validated.Metadata.OwnerProject = req.Project
	validated.Metadata.OwnerEnvironment = req.Environment
	validated.Metadata.DNSProvider = req.DNSProvider
	validated.Metadata.CAProvider = certificate.CAProvider
	validated.Metadata.Staging = certificate.Staging
	validated.Metadata.LastAttemptAt = timePointer(now)
	validated.Metadata.LastSuccessAt = timePointer(succeededAt)
	if err := publishProxyCertificate(ctx, validated, material.CertificatePEM, material.PrivateKeyPEM); err != nil {
		return ACMEDNSCertificateResult{}, err
	}
	_ = os.Remove(acmeDNSFailurePath(certificate.Domain))
	return ACMEDNSCertificateResult{Domain: certificate.Domain, Action: "issued", Certificate: validated.Metadata}, nil
}

func validateACMEDNSReconcileRequest(req *ACMEDNSReconcileRequest) error {
	if req == nil || !isSafeProjectName(req.Project) || !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid ACME DNS project/environment")
	}
	req.DNSProvider = strings.ToLower(strings.TrimSpace(req.DNSProvider))
	allowedCredentials := map[string]map[string]bool{
		ACMEDNSProviderCloudflare:   {"apiToken": true, "zoneToken": false},
		ACMEDNSProviderHetzner:      {"apiToken": true},
		ACMEDNSProviderDigitalOcean: {"apiToken": true},
	}
	required, ok := allowedCredentials[req.DNSProvider]
	if !ok {
		return fmt.Errorf("unsupported ACME DNS provider %q", req.DNSProvider)
	}
	for key, value := range req.Credentials {
		if _, ok := required[key]; !ok {
			return fmt.Errorf("unsupported %s credential %q", req.DNSProvider, key)
		}
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid %s credential %q", req.DNSProvider, key)
		}
	}
	for key, needed := range required {
		if needed && strings.TrimSpace(req.Credentials[key]) == "" {
			return fmt.Errorf("%s credential %s is required", req.DNSProvider, key)
		}
	}
	if len(req.Certificates) == 0 {
		return fmt.Errorf("at least one ACME DNS certificate is required")
	}
	seen := make(map[string]bool, len(req.Certificates))
	for i := range req.Certificates {
		certificate := &req.Certificates[i]
		domain, err := normalizeProxyCertificateDomain(certificate.Domain)
		if err != nil {
			return err
		}
		certificate.Domain = domain
		if seen[domain] {
			return fmt.Errorf("duplicate ACME DNS certificate %s", domain)
		}
		seen[domain] = true
		certificate.Email = strings.TrimSpace(certificate.Email)
		if certificate.Email != "" && !strings.Contains(certificate.Email, "@") {
			return fmt.Errorf("invalid ACME account email for %s", domain)
		}
		certificate.CAProvider = strings.ToLower(strings.TrimSpace(certificate.CAProvider))
		if certificate.CAProvider == "" {
			certificate.CAProvider = ACMEDNSCAProviderLetsEncrypt
		}
		if certificate.CAProvider != ACMEDNSCAProviderLetsEncrypt && certificate.CAProvider != ACMEDNSCAProviderZeroSSL {
			return fmt.Errorf("unsupported ACME CA provider %q", certificate.CAProvider)
		}
		if certificate.CAProvider == ACMEDNSCAProviderZeroSSL && certificate.Email == "" {
			return fmt.Errorf("ACME account email is required for ZeroSSL certificate %s", domain)
		}
	}
	sort.Slice(req.Certificates, func(i, j int) bool { return req.Certificates[i].Domain < req.Certificates[j].Domain })
	return nil
}

func obtainACMEDNSCertificate(ctx context.Context, providerName string, credentials map[string]string, certificate ACMEDNSCertificateRequest) (acmeDNSCertificateMaterial, error) {
	provider, err := acmeDNSProviderFactory(providerName, credentials)
	if err != nil {
		return acmeDNSCertificateMaterial{}, err
	}
	storage := &certmagic.FileStorage{Path: acmeDNSStoragePath()}
	logger := zap.NewNop()
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert:   func(certmagic.Certificate) (*certmagic.Config, error) { return magic, nil },
		RenewCheckInterval: certificateRenewCheckInterval,
		OCSPCheckInterval:  365 * 24 * time.Hour,
		Logger:             logger,
	})
	// Tako's scheduler is the sole renewal driver. Stop CertMagic's cache
	// maintenance goroutine before using the config for foreground issuance.
	cache.Stop()
	magic = certmagic.New(cache, certmagic.Config{Storage: storage, Logger: logger})
	runtimeOptions := acmeDNSIssuerRuntime()
	caURL := certmagic.LetsEncryptProductionCA
	if certificate.Staging {
		caURL = certmagic.LetsEncryptStagingCA
	} else if certificate.CAProvider == ACMEDNSCAProviderZeroSSL {
		caURL = certmagic.ZeroSSLProductionCA
	}
	if runtimeOptions.CAURL != "" {
		caURL = runtimeOptions.CAURL
	}
	propagationTimeout := 2 * time.Minute
	if runtimeOptions.PropagationTimeout != 0 {
		propagationTimeout = runtimeOptions.PropagationTimeout
	}
	issuerTemplate := certmagic.ACMEIssuer{
		CA:           caURL,
		Email:        certificate.Email,
		Agreed:       true,
		TrustedRoots: runtimeOptions.TrustedRoots,
		DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider:        provider,
			PropagationTimeout: propagationTimeout,
			Resolvers:          append([]string(nil), runtimeOptions.Resolvers...),
			Logger:             logger,
		}},
	}
	if certificate.CAProvider == ACMEDNSCAProviderZeroSSL && !certificate.Staging {
		issuerTemplate.NewAccountFunc = zeroSSLACMENewAccount(certificate.Email)
	}
	issuer := certmagic.NewACMEIssuer(magic, issuerTemplate)
	magic.Issuers = []certmagic.Issuer{issuer}
	certKey := certmagic.StorageKeys.SiteCert(issuer.IssuerKey(), certificate.Domain)
	privateKey := certmagic.StorageKeys.SitePrivateKey(issuer.IssuerKey(), certificate.Domain)
	hadExisting := storage.Exists(ctx, certKey) && storage.Exists(ctx, privateKey)
	var previousCert []byte
	if hadExisting {
		previousCert, _ = storage.Load(ctx, certKey)
		if runtimeOptions.ForceRenew {
			err = magic.RenewCertSync(ctx, certificate.Domain, true)
		} else {
			// ManageSync refreshes ARI in the foreground before CertMagic
			// decides whether renewal is due. The async cache loop remains
			// stopped so takod is still the sole scheduler.
			err = acmeDNSManageSync(ctx, magic, []string{certificate.Domain})
		}
		if err != nil {
			return acmeDNSCertificateMaterial{}, err
		}
	} else if err := magic.ObtainCertSync(ctx, certificate.Domain); err != nil {
		return acmeDNSCertificateMaterial{}, err
	}
	certPEM, err := storage.Load(ctx, certmagic.StorageKeys.SiteCert(issuer.IssuerKey(), certificate.Domain))
	if err != nil {
		return acmeDNSCertificateMaterial{}, fmt.Errorf("failed to load issued certificate: %w", err)
	}
	keyPEM, err := storage.Load(ctx, certmagic.StorageKeys.SitePrivateKey(issuer.IssuerKey(), certificate.Domain))
	if err != nil {
		return acmeDNSCertificateMaterial{}, fmt.Errorf("failed to load issued private key: %w", err)
	}
	return acmeDNSCertificateMaterial{CertificatePEM: certPEM, PrivateKeyPEM: keyPEM, Changed: !hadExisting || !bytes.Equal(previousCert, certPEM)}, nil
}

func zeroSSLACMENewAccount(email string) func(context.Context, *certmagic.ACMEIssuer, acme.Account) (acme.Account, error) {
	return func(ctx context.Context, issuer *certmagic.ACMEIssuer, account acme.Account) (acme.Account, error) {
		if issuer.ExternalAccount != nil {
			return account, nil
		}
		if strings.TrimSpace(email) == "" {
			return account, fmt.Errorf("email is required to use ZeroSSL")
		}
		if len(account.Contact) == 0 {
			account.Contact = []string{"mailto:" + email}
		}
		form := url.Values{"email": []string{email}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, zerossl.BaseURL+"/acme/eab-credentials-email", strings.NewReader(form.Encode()))
		if err != nil {
			return account, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return account, fmt.Errorf("getting ZeroSSL EAB credentials: %w", err)
		}
		defer response.Body.Close()
		var result struct {
			Error struct {
				Code int    `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
			KeyID  string `json:"eab_kid"`
			MACKey string `json:"eab_hmac_key"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			return account, fmt.Errorf("decoding ZeroSSL EAB response: %w", err)
		}
		if response.StatusCode != http.StatusOK || result.Error.Code != 0 || result.KeyID == "" || result.MACKey == "" {
			return account, fmt.Errorf("getting ZeroSSL EAB credentials failed: HTTP %d %s (code %d)", response.StatusCode, result.Error.Type, result.Error.Code)
		}
		issuer.ExternalAccount = &acme.EAB{KeyID: result.KeyID, MACKey: result.MACKey}
		return account, nil
	}
}

func newACMEDNSProvider(name string, credentials map[string]string) (certmagic.DNSProvider, error) {
	switch name {
	case ACMEDNSProviderCloudflare:
		return &cloudflare.Provider{APIToken: credentials["apiToken"], ZoneToken: credentials["zoneToken"]}, nil
	case ACMEDNSProviderHetzner:
		return hetzner.New(credentials["apiToken"]), nil
	case ACMEDNSProviderDigitalOcean:
		return &digitalocean.Provider{APIToken: credentials["apiToken"]}, nil
	default:
		return nil, fmt.Errorf("unsupported ACME DNS provider %q", name)
	}
}

func classifyACMEDNSFailure(project, environment, domain string, now time.Time, err error) acmeDNSFailureDocument {
	code := "issuance_failed"
	retryAfter := now.Add(acmeDNSFailureCooldown)
	var problem acme.Problem
	if errors.As(err, &problem) && strings.HasSuffix(strings.ToLower(problem.Type), "ratelimited") {
		code = "rate_limited"
		retryAfter = now.Add(acmeDNSRateLimitBackoff)
		if parsed := parseRetryAfter(problem.Detail, now); parsed.After(now) {
			retryAfter = parsed
		}
	}
	return acmeDNSFailureDocument{
		Domain: domain, Code: code, FailedAt: now, RetryAfter: retryAfter,
		Error: err.Error(), Project: project, Environment: environment,
	}
}

func parseRetryAfter(detail string, fallback time.Time) time.Time {
	lower := strings.ToLower(detail)
	if index := strings.Index(lower, "retry-after"); index >= 0 {
		detail = detail[index+len("retry-after"):]
	} else if index := strings.Index(lower, "retry after"); index >= 0 {
		detail = detail[index+len("retry after"):]
	}
	detail = strings.TrimSpace(strings.TrimLeft(detail, ":="))
	if parsed, err := http.ParseTime(strings.Trim(detail, " (),.;\"'")); err == nil {
		return parsed.UTC()
	}
	for _, field := range strings.Fields(detail) {
		candidate := strings.Trim(field, "(),.;\"'")
		if parsed, err := time.Parse(time.RFC3339, candidate); err == nil {
			return parsed.UTC()
		}
		if seconds, err := strconv.Atoi(candidate); err == nil && seconds >= 0 {
			return fallback.Add(time.Duration(seconds) * time.Second).UTC()
		}
	}
	return fallback
}

func ensureACMEDNSDirectories() error {
	for _, path := range []string{
		acmeDNSRootPath(), acmeDNSStoragePath(),
		filepath.Join(acmeDNSRootPath(), acmeDNSCredentialsDir),
		filepath.Join(acmeDNSRootPath(), acmeDNSOwnersDir),
		filepath.Join(acmeDNSRootPath(), acmeDNSPendingDir),
		filepath.Join(acmeDNSRootPath(), acmeDNSFailuresDir),
	} {
		if err := ensureSecureProxyCertificateDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

func acmeDNSRootPath() string    { return filepath.Join(proxyCertStoreDir, ".acme") }
func acmeDNSStoragePath() string { return filepath.Join(acmeDNSRootPath(), "storage") }

func acmeDNSCredentialsPath(project, environment string) string {
	return filepath.Join(acmeDNSRootPath(), acmeDNSCredentialsDir, project, environment+".json")
}

func acmeDNSOwnerPath(project, environment string) string {
	return filepath.Join(acmeDNSRootPath(), acmeDNSOwnersDir, project, environment+".json")
}

func acmeDNSPendingPath(project, environment string) string {
	return filepath.Join(acmeDNSRootPath(), acmeDNSPendingDir, project, environment+".json")
}

func acmeDNSFailurePath(domain string) string {
	return filepath.Join(acmeDNSRootPath(), acmeDNSFailuresDir, certmagic.StorageKeys.Safe(domain)+".json")
}

func writePrivateJSON(path string, value any) error {
	if err := ensureSecureProxyCertificateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0600)
}

func readACMEDNSFailure(domain string) (*acmeDNSFailureDocument, error) {
	data, err := os.ReadFile(acmeDNSFailurePath(domain))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var failure acmeDNSFailureDocument
	if err := json.Unmarshal(data, &failure); err != nil {
		return nil, fmt.Errorf("invalid ACME DNS failure state for %s: %w", domain, err)
	}
	return &failure, nil
}

func loadACMEDNSFailures() ([]acmeDNSFailureDocument, error) {
	root := filepath.Join(acmeDNSRootPath(), acmeDNSFailuresDir)
	files, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	failures := make([]acmeDNSFailureDocument, 0, len(files))
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			return nil, fmt.Errorf("invalid ACME DNS failure file %q", file.Name())
		}
		data, err := readProxyCertificateStoreFile(filepath.Join(root, file.Name()))
		if err != nil {
			return nil, err
		}
		var failure acmeDNSFailureDocument
		if err := json.Unmarshal(data, &failure); err != nil {
			return nil, fmt.Errorf("invalid ACME DNS failure file %s: %w", file.Name(), err)
		}
		domain, err := normalizeProxyCertificateDomain(failure.Domain)
		if err != nil || !isSafeProjectName(failure.Project) || !isSafeRuntimeName(failure.Environment) {
			return nil, fmt.Errorf("invalid ACME DNS failure identity in %s", file.Name())
		}
		failure.Domain = domain
		failures = append(failures, failure)
	}
	return failures, nil
}

func readACMEDNSPending(project, environment string) (*acmeDNSPendingDocument, error) {
	data, err := readProxyCertificateStoreFile(acmeDNSPendingPath(project, environment))
	if err != nil {
		return nil, fmt.Errorf("failed to read staged ACME DNS configuration: %w", err)
	}
	var pending acmeDNSPendingDocument
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("invalid staged ACME DNS configuration: %w", err)
	}
	if err := validateACMEDNSPendingDocument(&pending); err != nil {
		return nil, err
	}
	if pending.Owner.Project != project || pending.Owner.Environment != environment {
		return nil, fmt.Errorf("staged ACME DNS configuration identity mismatch")
	}
	return &pending, nil
}

func validateACMEDNSPendingDocument(pending *acmeDNSPendingDocument) error {
	if pending == nil || pending.Credentials.DNSProvider != pending.Owner.DNSProvider {
		return fmt.Errorf("staged ACME DNS configuration provider mismatch")
	}
	request := ACMEDNSReconcileRequest{
		Project: pending.Owner.Project, Environment: pending.Owner.Environment,
		DNSProvider: pending.Owner.DNSProvider, Credentials: cloneStringMap(pending.Credentials.Credentials),
		Certificates: append([]ACMEDNSCertificateRequest(nil), pending.Owner.Certificates...),
	}
	if err := validateACMEDNSReconcileRequest(&request); err != nil {
		return fmt.Errorf("invalid staged ACME DNS configuration: %w", err)
	}
	pending.Owner.DNSProvider = request.DNSProvider
	pending.Owner.Certificates = request.Certificates
	pending.Credentials.DNSProvider = request.DNSProvider
	return nil
}

func loadACMEDNSOwnerClaims() ([]acmeDNSOwnerClaim, error) {
	owners, err := loadACMEDNSOwnerDocuments()
	if err != nil {
		return nil, err
	}
	var claims []acmeDNSOwnerClaim
	for _, owner := range owners {
		for _, certificate := range owner.Certificates {
			claims = append(claims, acmeDNSOwnerClaim{Domain: certificate.Domain, Project: owner.Project, Environment: owner.Environment})
		}
	}
	sort.Slice(claims, func(i, j int) bool {
		if len(claims[i].Domain) != len(claims[j].Domain) {
			return len(claims[i].Domain) > len(claims[j].Domain)
		}
		return claims[i].Domain < claims[j].Domain
	})
	return claims, nil
}

func loadACMEDNSOwnerDocuments() ([]acmeDNSOwnerDocument, error) {
	active, err := loadActiveACMEDNSOwnerDocuments()
	if err != nil {
		return nil, err
	}
	pending, err := loadPendingACMEDNSOwnerDocuments()
	if err != nil {
		return nil, err
	}
	owners := append(active, pending...)
	sort.Slice(owners, func(i, j int) bool {
		left := owners[i].Project + "/" + owners[i].Environment
		right := owners[j].Project + "/" + owners[j].Environment
		if left == right {
			return owners[i].UpdatedAt.Before(owners[j].UpdatedAt)
		}
		return left < right
	})
	return owners, nil
}

func loadActiveACMEDNSOwnerDocuments() ([]acmeDNSOwnerDocument, error) {
	return loadACMEDNSOwnerDocumentsFromDir(filepath.Join(acmeDNSRootPath(), acmeDNSOwnersDir))
}

func loadPendingACMEDNSOwnerDocuments() ([]acmeDNSOwnerDocument, error) {
	root := filepath.Join(acmeDNSRootPath(), acmeDNSPendingDir)
	projects, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var owners []acmeDNSOwnerDocument
	for _, projectEntry := range projects {
		if !projectEntry.IsDir() || !isSafeProjectName(projectEntry.Name()) {
			return nil, fmt.Errorf("invalid ACME DNS pending directory %q", projectEntry.Name())
		}
		files, err := os.ReadDir(filepath.Join(root, projectEntry.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				return nil, fmt.Errorf("invalid ACME DNS pending file %q", file.Name())
			}
			data, err := readProxyCertificateStoreFile(filepath.Join(root, projectEntry.Name(), file.Name()))
			if err != nil {
				return nil, err
			}
			var pending acmeDNSPendingDocument
			if err := json.Unmarshal(data, &pending); err != nil {
				return nil, fmt.Errorf("invalid ACME DNS pending file %s: %w", file.Name(), err)
			}
			if err := validateACMEDNSPendingDocument(&pending); err != nil {
				return nil, fmt.Errorf("invalid ACME DNS pending file %s: %w", file.Name(), err)
			}
			if pending.Owner.Project != projectEntry.Name() || file.Name() != pending.Owner.Environment+".json" {
				return nil, fmt.Errorf("invalid ACME DNS pending identity in %s", file.Name())
			}
			owners = append(owners, pending.Owner)
		}
	}
	return owners, nil
}

func loadACMEDNSOwnerDocumentsFromDir(root string) ([]acmeDNSOwnerDocument, error) {
	projects, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var owners []acmeDNSOwnerDocument
	for _, projectEntry := range projects {
		if !projectEntry.IsDir() || !isSafeProjectName(projectEntry.Name()) {
			return nil, fmt.Errorf("invalid ACME DNS owner directory %q", projectEntry.Name())
		}
		files, err := os.ReadDir(filepath.Join(root, projectEntry.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				return nil, fmt.Errorf("invalid ACME DNS owner file %q", file.Name())
			}
			data, err := readProxyCertificateStoreFile(filepath.Join(root, projectEntry.Name(), file.Name()))
			if err != nil {
				return nil, err
			}
			var owner acmeDNSOwnerDocument
			if err := json.Unmarshal(data, &owner); err != nil {
				return nil, fmt.Errorf("invalid ACME DNS owner file %s: %w", file.Name(), err)
			}
			if owner.Project != projectEntry.Name() || !isSafeRuntimeName(owner.Environment) {
				return nil, fmt.Errorf("invalid ACME DNS owner identity in %s", file.Name())
			}
			for i := range owner.Certificates {
				certificate := &owner.Certificates[i]
				domain, err := normalizeProxyCertificateDomain(certificate.Domain)
				if err != nil {
					return nil, err
				}
				certificate.Domain = domain
			}
			owners = append(owners, owner)
		}
	}
	sort.Slice(owners, func(i, j int) bool {
		return owners[i].Project+"/"+owners[i].Environment < owners[j].Project+"/"+owners[j].Environment
	})
	return owners, nil
}

func selectACMEDNSOwner(claims []acmeDNSOwnerClaim, host string) *acmeDNSOwnerClaim {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for i := range claims {
		claim := &claims[i]
		if claim.Domain == host || wildcardCoversHost(claim.Domain, host) {
			return claim
		}
	}
	return nil
}

func wildcardCoversHost(pattern, host string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := strings.TrimPrefix(pattern, "*")
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := strings.TrimSuffix(host, suffix)
	return label != "" && !strings.Contains(label, ".")
}

func loadExactProxyCertificate(domain string, requireCurrent bool) (*proxyCertificateEntry, error) {
	path := filepath.Join(proxyCertStoreDir, domain)
	if _, err := os.Lstat(path); err != nil {
		return nil, err
	}
	return loadProxyCertificateEntry(path, domain, requireCurrent)
}

func updateProxyCertificateMetadata(domain string, metadata ProxyCertificateMetadata) error {
	entryPath := filepath.Join(proxyCertStoreDir, domain)
	if _, err := proxyCertificateLinkTarget(entryPath); err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(entryPath, proxyCertMetadataFile), append(data, '\n'), 0600)
}

func recordACMEDNSAttemptMetadata(domain string, attempted time.Time, succeeded *time.Time, message string, retryAfter *time.Time) error {
	entry, err := loadExactProxyCertificate(domain, false)
	if err != nil {
		return err
	}
	entry.Metadata.LastAttemptAt = timePointer(attempted)
	entry.Metadata.LastSuccessAt = succeeded
	entry.Metadata.LastError = message
	entry.Metadata.RetryAfter = retryAfter
	entry.Metadata.UpdatedAt = attempted
	return updateProxyCertificateMetadata(domain, entry.Metadata)
}

func reconcileACMEDNSOrphans(project, environment string, owned map[string]bool) error {
	_, err := markACMEDNSCertificatesOrphaned(project, environment, owned)
	return err
}

func markACMEDNSCertificatesOrphaned(project, environment string, owned map[string]bool) ([]string, error) {
	entries, err := loadProxyCertificateEntries(false)
	if err != nil {
		return nil, err
	}
	var orphaned []string
	for _, entry := range entries {
		metadata := entry.Metadata
		if metadata.Source != CertificateSourceACMEDNS || metadata.OwnerProject != project || metadata.OwnerEnvironment != environment {
			continue
		}
		shouldOrphan := owned == nil || !owned[metadata.Domain]
		if metadata.Orphaned == shouldOrphan {
			if shouldOrphan {
				orphaned = append(orphaned, metadata.Domain)
			}
			continue
		}
		metadata.Orphaned = shouldOrphan
		metadata.UpdatedAt = acmeDNSNow().UTC()
		if err := updateProxyCertificateMetadata(metadata.Domain, metadata); err != nil {
			return nil, err
		}
		if shouldOrphan {
			orphaned = append(orphaned, metadata.Domain)
		}
	}
	sort.Strings(orphaned)
	return orphaned, nil
}

func ownedDomainSet(certificates []ACMEDNSCertificateRequest) map[string]bool {
	result := make(map[string]bool, len(certificates))
	for _, certificate := range certificates {
		result[certificate.Domain] = true
	}
	return result
}

func redactACMEDNSError(err error, credentials map[string]string) error {
	if err == nil {
		return nil
	}
	message := redactACMEText(err.Error(), credentials)
	var typed *ACMEDNSError
	if errors.As(err, &typed) {
		return &ACMEDNSError{Code: typed.Code, Domain: typed.Domain, RetryAfter: typed.RetryAfter, Completed: append([]ACMEDNSCertificateResult(nil), typed.Completed...), Err: errors.New(message)}
	}
	return errors.New(message)
}

func redactACMEText(message string, credentials map[string]string) string {
	for _, value := range credentials {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	return message
}

func timePointer(value time.Time) *time.Time {
	copy := value.UTC()
	return &copy
}
