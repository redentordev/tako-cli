package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

var (
	certificateRenewCheckInterval = 24 * time.Hour
	certificateRenewInitialDelay  = time.Minute
	certificateRenewTimeout       = 5 * time.Minute
)

// CertificateScheduler is takod's sole renewal driver. CertMagic cache
// maintenance is stopped before foreground operations; this daily loop is the
// only component allowed to renew managed certificates.
type CertificateScheduler struct {
	dataDir string
	admit   func(...string) error
}

type certificateStateEventDocument struct {
	APIVersion    string            `json:"apiVersion"`
	Kind          string            `json:"kind"`
	SchemaVersion int               `json:"schemaVersion"`
	Type          string            `json:"type"`
	Project       string            `json:"project"`
	Environment   string            `json:"environment"`
	Message       string            `json:"message,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
	Time          time.Time         `json:"time"`
}

func NewCertificateScheduler(dataDir string) *CertificateScheduler {
	return &CertificateScheduler{dataDir: dataDir}
}

func (s *CertificateScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	timer := time.NewTimer(certificateRenewInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.Check(ctx); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "takod certificate renewal check failed: %v\n", err)
			}
			timer.Reset(certificateRenewCheckInterval)
		}
	}
}

func (s *CertificateScheduler) Check(ctx context.Context) error {
	if s.admit != nil {
		if err := s.admit(proxyDynamicDir, proxyCertStoreDir, s.dataDir); err != nil {
			return fmt.Errorf("certificate scheduler denied by resource admission: %w", err)
		}
	}
	manifests, err := readProxyRouteManifests(proxyRoutesDir)
	if err != nil {
		return err
	}
	if err := recoverOrDiscardPendingACMEDNS(ctx, manifests); err != nil {
		return err
	}
	owners, err := loadActiveACMEDNSOwnerDocuments()
	if err != nil {
		return err
	}
	owned := make(map[string]bool)
	for _, owner := range owners {
		referenced := acmeDNSOwnerRouteReferences(owner, manifests)
		if len(referenced) == 0 && !owner.UpdatedAt.IsZero() && acmeDNSNow().UTC().Sub(owner.UpdatedAt) < 10*time.Minute {
			for _, certificate := range owner.Certificates {
				owned[certificate.Domain] = true
			}
			continue // deploy may still be between issue-first and manifest publish
		}
		for domain := range referenced {
			owned[domain] = true
		}
		if len(referenced) == 0 {
			continue
		}
		for _, certificate := range owner.Certificates {
			if !referenced[certificate.Domain] {
				continue
			}
			if err := s.checkOwnedCertificate(ctx, owner, certificate); err != nil {
				fmt.Fprintf(os.Stderr, "takod certificate renewal failed for %s: %v\n", certificate.Domain, err)
			}
		}
	}
	return markAllUnownedACMECertificatesOrphaned(owned)
}

func recoverOrDiscardPendingACMEDNS(ctx context.Context, manifests []ProxyRouteManifest) error {
	pendingOwners, err := loadPendingACMEDNSOwnerDocuments()
	if err != nil {
		return err
	}
	now := acmeDNSNow().UTC()
	for _, owner := range pendingOwners {
		if len(acmeDNSOwnerRouteReferences(owner, manifests)) == len(owner.Certificates) {
			if _, err := FinalizeACMEDNS(ctx, owner.Project, owner.Environment); err != nil {
				return fmt.Errorf("failed to recover published ACME DNS ownership for %s/%s: %w", owner.Project, owner.Environment, err)
			}
			continue
		}
		if owner.UpdatedAt.IsZero() || now.Sub(owner.UpdatedAt) <= acmeDNSPendingTTL {
			continue
		}
		proxyCertificateMu.Lock()
		pending, readErr := readACMEDNSPending(owner.Project, owner.Environment)
		if readErr == nil && now.Sub(pending.Owner.UpdatedAt) > acmeDNSPendingTTL {
			readErr = os.Remove(acmeDNSPendingPath(owner.Project, owner.Environment))
			if os.IsNotExist(readErr) {
				readErr = nil
			}
			activeOwners, activeErr := loadActiveACMEDNSOwnerDocuments()
			if readErr == nil && activeErr == nil {
				activeDomains := map[string]bool{}
				for _, active := range activeOwners {
					if active.Project == owner.Project && active.Environment == owner.Environment {
						activeDomains = ownedDomainSet(active.Certificates)
						break
					}
				}
				_, readErr = markACMEDNSCertificatesOrphaned(owner.Project, owner.Environment, activeDomains)
			}
		}
		proxyCertificateMu.Unlock()
		if readErr != nil {
			return fmt.Errorf("failed to discard abandoned ACME DNS prepare for %s/%s: %w", owner.Project, owner.Environment, readErr)
		}
	}
	return nil
}

func acmeDNSOwnerRouteReferences(owner acmeDNSOwnerDocument, manifests []ProxyRouteManifest) map[string]bool {
	referenced := make(map[string]bool)
	owned := make(map[string]bool, len(owner.Certificates))
	for _, certificate := range owner.Certificates {
		owned[certificate.Domain] = true
	}
	for _, manifest := range manifests {
		if manifest.Project != owner.Project || manifest.Environment != owner.Environment {
			continue
		}
		for _, route := range manifest.Routes {
			for _, domain := range append(append([]string(nil), route.Domains...), route.RedirectFrom...) {
				if owned[domain] {
					referenced[domain] = true
				}
			}
		}
	}
	return referenced
}

func (s *CertificateScheduler) checkOwnedCertificate(ctx context.Context, owner acmeDNSOwnerDocument, certificate ACMEDNSCertificateRequest) error {
	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	activeOwners, err := loadActiveACMEDNSOwnerDocuments()
	if err != nil {
		return err
	}
	ownerStillActive := false
	for _, active := range activeOwners {
		if active.Project != owner.Project || active.Environment != owner.Environment {
			continue
		}
		for _, activeCertificate := range active.Certificates {
			if activeCertificate.Domain == certificate.Domain {
				owner = active
				certificate = activeCertificate
				ownerStillActive = true
				break
			}
		}
	}
	if !ownerStillActive {
		return nil
	}
	manifests, err := readProxyRouteManifests(proxyRoutesDir)
	if err != nil {
		return err
	}
	if !acmeDNSOwnerRouteReferences(owner, manifests)[certificate.Domain] {
		return nil
	}
	entry, err := loadExactProxyCertificate(certificate.Domain, false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if entry.Metadata.Source != CertificateSourceACMEDNS || entry.Metadata.OwnerProject != owner.Project || entry.Metadata.OwnerEnvironment != owner.Environment || entry.Metadata.Orphaned {
		return nil
	}
	now := acmeDNSNow().UTC()
	credentials, err := readACMEDNSCredentials(owner.Project, owner.Environment)
	if err != nil {
		message := redactACMEText(err.Error(), nil)
		retryAfter := now.Add(acmeDNSFailureCooldown)
		_ = recordACMEDNSAttemptMetadata(certificate.Domain, now, nil, message, &retryAfter)
		request := ACMEDNSReconcileRequest{Project: owner.Project, Environment: owner.Environment, DNSProvider: owner.DNSProvider}
		s.appendRenewalEvent(request, certificate.Domain, events.TypeCertRenewFailed, message, acmeDNSFailureDocument{Code: "credential_unavailable", RetryAfter: retryAfter})
		return err
	}
	request := ACMEDNSReconcileRequest{
		Project: owner.Project, Environment: owner.Environment, DNSProvider: owner.DNSProvider,
		Credentials: credentials.Credentials, Certificates: []ACMEDNSCertificateRequest{certificate},
	}
	if err := validateACMEDNSReconcileRequest(&request); err != nil {
		message := redactACMEText(err.Error(), request.Credentials)
		retryAfter := now.Add(acmeDNSFailureCooldown)
		_ = recordACMEDNSAttemptMetadata(certificate.Domain, now, nil, message, &retryAfter)
		s.appendRenewalEvent(request, certificate.Domain, events.TypeCertRenewFailed, message, acmeDNSFailureDocument{Code: "configuration_invalid", RetryAfter: retryAfter})
		return errors.New(message)
	}
	if failure, err := readACMEDNSFailure(certificate.Domain); err != nil {
		return err
	} else if failure != nil && now.Before(failure.RetryAfter) {
		return nil
	}

	operationCtx, cancel := context.WithTimeout(ctx, certificateRenewTimeout)
	material, renewErr := acmeDNSObtain(operationCtx, request.DNSProvider, request.Credentials, certificate)
	cancel()
	if renewErr != nil {
		failure := classifyACMEDNSFailure(request.Project, request.Environment, certificate.Domain, now, renewErr)
		failure.DNSProvider = request.DNSProvider
		failure.CAProvider = certificate.CAProvider
		failure.Staging = certificate.Staging
		failure.Error = redactACMEText(failure.Error, request.Credentials)
		_ = writePrivateJSON(acmeDNSFailurePath(certificate.Domain), failure)
		_ = recordACMEDNSAttemptMetadata(certificate.Domain, now, nil, failure.Error, &failure.RetryAfter)
		s.appendRenewalEvent(request, certificate.Domain, events.TypeCertRenewFailed, failure.Error, failure)
		return redactACMEDNSError(renewErr, request.Credentials)
	}
	if !material.Changed {
		return nil
	}

	succeededAt := acmeDNSNow().UTC()
	renewed, err := validateProxyCertificatePair(certificate.Domain, material.CertificatePEM, material.PrivateKeyPEM, CertificateSourceACMEDNS, succeededAt, true)
	if err != nil {
		return fmt.Errorf("renewed certificate failed store validation: %w", err)
	}
	renewed.Metadata.OwnerProject = request.Project
	renewed.Metadata.OwnerEnvironment = request.Environment
	renewed.Metadata.DNSProvider = request.DNSProvider
	renewed.Metadata.CAProvider = certificate.CAProvider
	renewed.Metadata.Staging = certificate.Staging
	renewed.Metadata.LastAttemptAt = timePointer(now)
	renewed.Metadata.LastSuccessAt = timePointer(succeededAt)
	if err := publishProxyCertificate(ctx, renewed, material.CertificatePEM, material.PrivateKeyPEM); err != nil {
		return err
	}
	_ = os.Remove(acmeDNSFailurePath(certificate.Domain))
	s.appendRenewalEvent(request, certificate.Domain, events.TypeCertRenewCompleted, "certificate renewal completed", acmeDNSFailureDocument{})
	return nil
}

func (s *CertificateScheduler) appendRenewalEvent(request ACMEDNSReconcileRequest, domain, eventType, message string, failure acmeDNSFailureDocument) {
	document := certificateStateEventDocument{
		APIVersion: events.APIVersionCurrent, Kind: "StateEvent", SchemaVersion: 1,
		Type: eventType, Project: request.Project, Environment: request.Environment,
		Time: acmeDNSNow().UTC(), Message: message,
		Details: map[string]string{"domain": domain, "dnsProvider": request.DNSProvider},
	}
	if !failure.RetryAfter.IsZero() {
		document.Details["retryAfter"] = failure.RetryAfter.UTC().Format(time.RFC3339)
		document.Details["errorClass"] = failure.Code
	}
	data, err := json.Marshal(document)
	if err != nil {
		return
	}
	_, _ = AppendStateEvent(context.Background(), s.dataDir, StateDocumentRequest{
		Project: request.Project, Environment: request.Environment, Content: string(data),
	})
}

func readACMEDNSCredentials(project, environment string) (*acmeDNSCredentialDocument, error) {
	data, err := readProxyCertificateStoreFile(acmeDNSCredentialsPath(project, environment))
	if err != nil {
		return nil, err
	}
	var credentials acmeDNSCredentialDocument
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, fmt.Errorf("invalid ACME credentials document: %w", err)
	}
	return &credentials, nil
}

func markAllUnownedACMECertificatesOrphaned(owned map[string]bool) error {
	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	entries, err := loadProxyCertificateEntries(false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Metadata.Source != CertificateSourceACMEDNS {
			continue
		}
		orphaned := !owned[entry.Metadata.Domain]
		if entry.Metadata.Orphaned == orphaned {
			continue
		}
		entry.Metadata.Orphaned = orphaned
		entry.Metadata.UpdatedAt = acmeDNSNow().UTC()
		if err := updateProxyCertificateMetadata(entry.Metadata.Domain, entry.Metadata); err != nil {
			return err
		}
	}
	return nil
}
