package takod

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CertificateSourcePushed  = "pushed"
	CertificateSourceACMEDNS = "acme-dns"

	proxyCertificateFile  = "cert.pem"
	proxyPrivateKeyFile   = "key.pem"
	proxyCertMetadataFile = "metadata.json"
	proxyCertVersionsDir  = ".versions"
)

var (
	proxyCertStoreDir           = "/var/lib/tako/certs"
	proxyCertContainerDir       = "/var/lib/tako/certs"
	proxyCertificateNow         = time.Now
	proxyCertificateMu          sync.Mutex
	proxyCertificateRender      = renderAndWriteCaddyfile
	proxyCertificatePublishLink = replaceProxyCertificateLink
)

type ProxyCertificatePushRequest struct {
	Domain  string `json:"domain"`
	CertPEM string `json:"certPem"`
	KeyPEM  string `json:"keyPem"`
}

type ProxyCertificateMetadata struct {
	Domain    string    `json:"domain"`
	Source    string    `json:"source"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	IssuedAt  time.Time `json:"issuedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type ProxyCertificateListResponse struct {
	Certificates []ProxyCertificateMetadata `json:"certificates"`
}

type ProxyCertificateMutationResponse struct {
	Certificate ProxyCertificateMetadata `json:"certificate"`
}

type proxyCertificateEntry struct {
	Metadata ProxyCertificateMetadata
	Leaf     *x509.Certificate
	CertPath string
	KeyPath  string
}

func PushProxyCertificate(ctx context.Context, req ProxyCertificatePushRequest) (*ProxyCertificateMutationResponse, error) {
	domain, err := normalizeProxyCertificateDomain(req.Domain)
	if err != nil {
		return nil, err
	}
	entry, err := validateProxyCertificatePair(domain, []byte(req.CertPEM), []byte(req.KeyPEM), CertificateSourcePushed, proxyCertificateNow().UTC(), true)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	if err := publishProxyCertificate(ctx, entry, []byte(req.CertPEM), []byte(req.KeyPEM)); err != nil {
		return nil, err
	}
	return &ProxyCertificateMutationResponse{Certificate: entry.Metadata}, nil
}

func ListProxyCertificates(ctx context.Context) (*ProxyCertificateListResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	entries, err := loadProxyCertificateEntries(false)
	if err != nil {
		return nil, err
	}
	result := &ProxyCertificateListResponse{Certificates: make([]ProxyCertificateMetadata, 0, len(entries))}
	for _, entry := range entries {
		result.Certificates = append(result.Certificates, entry.Metadata)
	}
	return result, nil
}

func RemoveProxyCertificate(ctx context.Context, domain string) (*ProxyCertificateMutationResponse, error) {
	domain, err := normalizeProxyCertificateDomain(domain)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	proxyCertificateMu.Lock()
	defer proxyCertificateMu.Unlock()
	if err := ensureSecureProxyCertificateDirectory(proxyCertStoreDir); err != nil {
		return nil, err
	}
	if err := ensureSecureProxyCertificateDirectory(filepath.Join(proxyCertStoreDir, proxyCertVersionsDir)); err != nil {
		return nil, err
	}
	target := filepath.Join(proxyCertStoreDir, domain)
	entry, err := loadProxyCertificateEntry(target, domain, false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("certificate %s not found", domain)
		}
		return nil, err
	}
	linkTarget, err := proxyCertificateLinkTarget(target)
	if err != nil {
		return nil, err
	}
	if err := renderAndWriteCaddyfileThen(ctx, domain, func() error {
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("failed to remove certificate link: %w", err)
		}
		if err := syncDirectory(proxyCertStoreDir); err != nil {
			return fmt.Errorf("failed to sync certificate store after removal: %w", err)
		}
		if err := os.RemoveAll(filepath.Join(proxyCertStoreDir, linkTarget)); err != nil {
			return fmt.Errorf("failed to remove certificate files: %w", err)
		}
		if err := syncDirectory(filepath.Join(proxyCertStoreDir, proxyCertVersionsDir)); err != nil {
			return fmt.Errorf("failed to sync certificate versions after removal: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &ProxyCertificateMutationResponse{Certificate: entry.Metadata}, nil
}

func publishProxyCertificate(ctx context.Context, entry *proxyCertificateEntry, certPEM []byte, keyPEM []byte) error {
	if err := ensureSecureProxyCertificateDirectory(proxyCertStoreDir); err != nil {
		return err
	}
	versionsDir := filepath.Join(proxyCertStoreDir, proxyCertVersionsDir)
	if err := ensureSecureProxyCertificateDirectory(versionsDir); err != nil {
		return fmt.Errorf("failed to create certificate store: %w", err)
	}
	staging, err := os.MkdirTemp(versionsDir, ".push-")
	if err != nil {
		return fmt.Errorf("failed to stage certificate: %w", err)
	}
	defer os.RemoveAll(staging)
	metadata, err := json.MarshalIndent(entry.Metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode certificate metadata: %w", err)
	}
	metadata = append(metadata, '\n')
	for name, content := range map[string][]byte{
		proxyCertificateFile:  certPEM,
		proxyPrivateKeyFile:   keyPEM,
		proxyCertMetadataFile: metadata,
	} {
		if err := writeFileAtomic(filepath.Join(staging, name), content, 0600); err != nil {
			return fmt.Errorf("failed to stage certificate %s: %w", name, err)
		}
	}

	versionName := entry.Metadata.Domain + "-" + strings.TrimPrefix(filepath.Base(staging), ".push-")
	versionPath := filepath.Join(versionsDir, versionName)
	if err := os.Rename(staging, versionPath); err != nil {
		return fmt.Errorf("failed to publish certificate version: %w", err)
	}
	if err := syncDirectory(versionsDir); err != nil {
		_ = os.RemoveAll(versionPath)
		return fmt.Errorf("failed to sync certificate version: %w", err)
	}
	relativeVersion := filepath.Join(proxyCertVersionsDir, versionName)
	target := filepath.Join(proxyCertStoreDir, entry.Metadata.Domain)
	oldLinkTarget := ""
	if _, err := os.Lstat(target); err == nil {
		oldLinkTarget, err = proxyCertificateLinkTarget(target)
		if err != nil {
			_ = os.RemoveAll(versionPath)
			return err
		}
	} else if !os.IsNotExist(err) {
		_ = os.RemoveAll(versionPath)
		return fmt.Errorf("failed to inspect existing certificate: %w", err)
	}
	if err := proxyCertificatePublishLink(target, relativeVersion); err != nil {
		_ = os.RemoveAll(versionPath)
		return err
	}
	if err := syncDirectory(proxyCertStoreDir); err != nil {
		rollbackErr := rollbackProxyCertificateLink(target, oldLinkTarget)
		if rollbackErr == nil {
			_ = os.RemoveAll(versionPath)
			_ = syncDirectory(versionsDir)
		}
		return fmt.Errorf("failed to sync active certificate link: %w; rollback: %v", err, rollbackErr)
	}
	if err := proxyCertificateRender(ctx); err != nil {
		rollbackErr := rollbackProxyCertificateLink(target, oldLinkTarget)
		if rollbackErr == nil {
			if removeErr := os.RemoveAll(versionPath); removeErr != nil {
				rollbackErr = fmt.Errorf("failed to remove rejected certificate version: %w", removeErr)
			} else if syncErr := syncDirectory(versionsDir); syncErr != nil {
				rollbackErr = fmt.Errorf("failed to sync rejected certificate removal: %w", syncErr)
			}
		}
		restoreRenderErr := error(nil)
		if rollbackErr == nil {
			restoreRenderErr = proxyCertificateRender(ctx)
		}
		if rollbackErr != nil || restoreRenderErr != nil {
			return fmt.Errorf("%w; certificate rollback failed: link=%v render=%v", err, rollbackErr, restoreRenderErr)
		}
		return err
	}
	if oldLinkTarget != "" {
		if err := os.RemoveAll(filepath.Join(proxyCertStoreDir, oldLinkTarget)); err != nil {
			return fmt.Errorf("certificate published but failed to remove replaced version: %w", err)
		}
		if err := syncDirectory(versionsDir); err != nil {
			return fmt.Errorf("certificate published but failed to sync replaced version removal: %w", err)
		}
	}
	return nil
}

func rollbackProxyCertificateLink(target string, oldLinkTarget string) error {
	if oldLinkTarget == "" {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove new active certificate link: %w", err)
		}
		if err := syncDirectory(proxyCertStoreDir); err != nil {
			return fmt.Errorf("failed to sync active certificate removal: %w", err)
		}
		return nil
	}
	if err := proxyCertificatePublishLink(target, oldLinkTarget); err != nil {
		return fmt.Errorf("failed to restore previous active certificate: %w", err)
	}
	if err := syncDirectory(proxyCertStoreDir); err != nil {
		return fmt.Errorf("failed to sync restored active certificate: %w", err)
	}
	return nil
}

func replaceProxyCertificateLink(target string, relativeVersion string) error {
	link, err := os.CreateTemp(proxyCertStoreDir, ".link-")
	if err != nil {
		return fmt.Errorf("failed to stage certificate link: %w", err)
	}
	linkPath := link.Name()
	if err := link.Close(); err != nil {
		_ = os.Remove(linkPath)
		return err
	}
	if err := os.Remove(linkPath); err != nil {
		return err
	}
	if err := os.Symlink(relativeVersion, linkPath); err != nil {
		return fmt.Errorf("failed to stage certificate link: %w", err)
	}
	defer os.Remove(linkPath)
	if err := os.Rename(linkPath, target); err != nil {
		return fmt.Errorf("failed to publish certificate link: %w", err)
	}
	return nil
}

func ensureSecureProxyCertificateDirectory(path string) error {
	info, err := os.Lstat(path)
	created := false
	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0700); err != nil {
			return fmt.Errorf("failed to create certificate directory %s: %w", path, err)
		}
		created = true
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("failed to inspect certificate directory %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("certificate directory %s must be a real directory", path)
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("failed to secure certificate directory %s: %w", path, err)
	}
	if err := os.Chown(path, os.Geteuid(), os.Getegid()); err != nil {
		return fmt.Errorf("failed to own certificate directory %s: %w", path, err)
	}
	if created {
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("failed to sync certificate directory creation for %s: %w", path, err)
		}
	}
	return nil
}

func proxyCertificateLinkTarget(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("certificate entry %s is not an atomic store link: %w", filepath.Base(path), err)
	}
	clean := filepath.Clean(target)
	if filepath.IsAbs(clean) || clean == proxyCertVersionsDir || !strings.HasPrefix(clean, proxyCertVersionsDir+string(filepath.Separator)) || strings.Contains(clean, "..") {
		return "", fmt.Errorf("certificate entry %s has unsafe store link %q", filepath.Base(path), target)
	}
	realStore, err := filepath.EvalSymlinks(proxyCertStoreDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve certificate store: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(filepath.Join(proxyCertStoreDir, clean))
	if err != nil {
		return "", fmt.Errorf("failed to resolve certificate entry %s: %w", filepath.Base(path), err)
	}
	relative, err := filepath.Rel(realStore, realTarget)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("certificate entry %s escapes the certificate store", filepath.Base(path))
	}
	return clean, nil
}

func validateProxyCertificatePair(domain string, certPEM []byte, keyPEM []byte, source string, now time.Time, requireCurrent bool) (*proxyCertificateEntry, error) {
	if source != CertificateSourcePushed && source != CertificateSourceACMEDNS {
		return nil, fmt.Errorf("invalid certificate source %q", source)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid or mismatched certificate/key pair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, fmt.Errorf("certificate PEM contains no certificates")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse leaf certificate: %w", err)
	}
	if requireCurrent && now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("certificate for %s is not valid until %s", domain, leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if requireCurrent && !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("certificate for %s expired at %s", domain, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if err := verifyCertificateClaim(leaf, domain); err != nil {
		return nil, err
	}
	return &proxyCertificateEntry{
		Metadata: ProxyCertificateMetadata{
			Domain: domain, Source: source, NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), IssuedAt: leaf.NotBefore.UTC(), UpdatedAt: now.UTC(),
		},
		Leaf: leaf,
	}, nil
}

func verifyCertificateClaim(leaf *x509.Certificate, domain string) error {
	if strings.HasPrefix(domain, "*.") {
		for _, name := range leaf.DNSNames {
			if strings.EqualFold(strings.TrimSuffix(name, "."), domain) {
				return nil
			}
		}
		return fmt.Errorf("certificate does not cover claimed wildcard %s", domain)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return fmt.Errorf("certificate does not cover claimed domain %s: %w", domain, err)
	}
	return nil
}

func normalizeProxyCertificateDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	base := strings.TrimPrefix(value, "*.")
	if value == "" || base == value && strings.Contains(value, "*") || base == "" || net.ParseIP(base) != nil || !isSafeRuntimeHost(base) {
		return "", fmt.Errorf("invalid certificate domain %q", value)
	}
	return value, nil
}

func loadProxyCertificateEntries(requireCurrent bool) ([]proxyCertificateEntry, error) {
	return loadProxyCertificateEntriesExcluding(requireCurrent, "")
}

func loadProxyCertificateEntriesExcluding(requireCurrent bool, excludedDomain string) ([]proxyCertificateEntry, error) {
	if _, err := os.Lstat(proxyCertStoreDir); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to inspect certificate store: %w", err)
	}
	if err := ensureSecureProxyCertificateDirectory(proxyCertStoreDir); err != nil {
		return nil, err
	}
	versionsDir := filepath.Join(proxyCertStoreDir, proxyCertVersionsDir)
	if _, err := os.Lstat(versionsDir); err == nil {
		if err := ensureSecureProxyCertificateDirectory(versionsDir); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to inspect certificate versions: %w", err)
	}
	dirs, err := os.ReadDir(proxyCertStoreDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read certificate store: %w", err)
	}
	entries := make([]proxyCertificateEntry, 0, len(dirs))
	for _, dir := range dirs {
		if strings.HasPrefix(dir.Name(), ".") {
			continue
		}
		if dir.Type()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("certificate store entry %q is not an atomic store link", dir.Name())
		}
		if dir.Name() == excludedDomain {
			continue
		}
		domain, err := normalizeProxyCertificateDomain(dir.Name())
		if err != nil || domain != dir.Name() {
			return nil, fmt.Errorf("invalid certificate store entry %q", dir.Name())
		}
		entryPath := filepath.Join(proxyCertStoreDir, dir.Name())
		if _, err := proxyCertificateLinkTarget(entryPath); err != nil {
			return nil, err
		}
		entry, err := loadProxyCertificateEntry(entryPath, domain, requireCurrent)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Metadata.Domain < entries[j].Metadata.Domain })
	return entries, nil
}

func loadProxyCertificateEntry(dir string, domain string, requireCurrent bool) (*proxyCertificateEntry, error) {
	linkTarget, err := proxyCertificateLinkTarget(dir)
	if err != nil {
		return nil, err
	}
	certPEM, err := readProxyCertificateStoreFile(filepath.Join(dir, proxyCertificateFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := readProxyCertificateStoreFile(filepath.Join(dir, proxyPrivateKeyFile))
	if err != nil {
		return nil, err
	}
	metadataBytes, err := readProxyCertificateStoreFile(filepath.Join(dir, proxyCertMetadataFile))
	if err != nil {
		return nil, err
	}
	var metadata ProxyCertificateMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("invalid certificate metadata for %s: %w", domain, err)
	}
	if metadata.Domain != domain {
		return nil, fmt.Errorf("certificate metadata domain %q does not match store entry %q", metadata.Domain, domain)
	}
	entry, err := validateProxyCertificatePair(domain, certPEM, keyPEM, metadata.Source, proxyCertificateNow().UTC(), requireCurrent)
	if err != nil {
		return nil, fmt.Errorf("invalid stored certificate %s: %w", domain, err)
	}
	entry.Metadata = metadata
	entry.Metadata.NotBefore = entry.Leaf.NotBefore.UTC()
	entry.Metadata.NotAfter = entry.Leaf.NotAfter.UTC()
	entry.CertPath = filepath.Join(proxyCertContainerDir, linkTarget, proxyCertificateFile)
	entry.KeyPath = filepath.Join(proxyCertContainerDir, linkTarget, proxyPrivateKeyFile)
	return entry, nil
}

func readProxyCertificateStoreFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("certificate store file %s must be a regular 0600 file", path)
	}
	return os.ReadFile(path)
}

func selectProxyCertificate(entries []proxyCertificateEntry, host string) *proxyCertificateEntry {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for i := range entries {
		if entries[i].Metadata.Domain == host {
			return &entries[i]
		}
	}
	var selected *proxyCertificateEntry
	for i := range entries {
		entry := &entries[i]
		if !strings.HasPrefix(entry.Metadata.Domain, "*.") || entry.Leaf.VerifyHostname(host) != nil {
			continue
		}
		if selected == nil || len(entry.Metadata.Domain) > len(selected.Metadata.Domain) {
			selected = entry
		}
	}
	return selected
}
