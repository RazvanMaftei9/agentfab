package identity

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/razvanmaftei/agentfab/internal/mtls"
)

const (
	defaultLocalDevTTL       = 24 * time.Hour
	localDevMaxLeafTTL       = 24 * time.Hour
	localDevIssuerDir        = "shared/identity/local-dev"
	localDevBundleStateFile  = "bundle.json"
	localDevCARotationWindow = 30 * 24 * time.Hour
	localDevRetiredRootTTL   = localDevMaxLeafTTL + time.Hour
)

type LocalDevProvider struct {
	trustDomain string
	dataDir     string

	mu sync.RWMutex
	ca *mtls.ClusterCA
}

type localDevTrustState struct {
	Retired []retiredLocalDevRoot `json:"retired,omitempty"`
}

type retiredLocalDevRoot struct {
	CertPEM             []byte `json:"cert_pem"`
	RetireAfterUnixNano int64  `json:"retire_after_unix_nano"`
}

type sharedLocalDevMaterials struct {
	activeCA *mtls.ClusterCA
	bundle   TrustBundle
}

func NewLocalDevProvider(trustDomain string) (*LocalDevProvider, error) {
	ca, err := mtls.NewClusterCA()
	if err != nil {
		return nil, err
	}
	return &LocalDevProvider{
		trustDomain: trustDomain,
		ca:          ca,
	}, nil
}

func NewSharedLocalDevProvider(dataDir, trustDomain string) (*LocalDevProvider, error) {
	materials, err := loadSharedLocalDevMaterials(dataDir, trustDomain)
	if err != nil {
		return nil, err
	}
	return &LocalDevProvider{
		trustDomain: trustDomain,
		dataDir:     dataDir,
		ca:          materials.activeCA,
	}, nil
}

func (p *LocalDevProvider) Name() string {
	return "local-dev"
}

func (p *LocalDevProvider) ClusterCA() *mtls.ClusterCA {
	if p.dataDir != "" {
		if materials, err := loadSharedLocalDevMaterials(p.dataDir, p.trustDomain); err == nil {
			p.mu.Lock()
			p.ca = materials.activeCA
			p.mu.Unlock()
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ca
}

func (p *LocalDevProvider) TrustBundle(_ context.Context) (TrustBundle, error) {
	materials, err := p.currentMaterials()
	if err != nil {
		return TrustBundle{}, err
	}
	return materials.bundle, nil
}

func (p *LocalDevProvider) IssueCertificate(_ context.Context, req IssueRequest) (IssuedCertificate, error) {
	if err := req.Subject.Validate(); err != nil {
		return IssuedCertificate{}, err
	}

	principal := req.Principal
	if principal == "" {
		principal = req.Subject.Name
	}
	if principal == "" {
		return IssuedCertificate{}, fmt.Errorf("principal is required")
	}

	materials, err := p.currentMaterials()
	if err != nil {
		return IssuedCertificate{}, err
	}

	options := mtls.IssueCertOptions{
		DNSNames:    uniqueStrings(req.DNSNames),
		IPAddresses: uniqueIPs(req.IPAddresses),
		IdentityURI: req.Subject.URI(),
		TTL:         requestedTTL(req.RequestedTTL),
	}
	cert, err := materials.activeCA.IssueCertWithOptions(principal, options)
	if err != nil {
		return IssuedCertificate{}, err
	}

	serverTLS := mtls.ServerTLSConfig(cert, materials.bundle.RootCAs)
	clientTLS := mtls.ClientTLSConfig(cert, materials.bundle.RootCAs)

	expiresAt := time.Now().Add(defaultLocalDevTTL)
	if len(cert.Certificate) > 0 {
		if leaf, parseErr := x509.ParseCertificate(cert.Certificate[0]); parseErr == nil {
			expiresAt = leaf.NotAfter
		}
	}

	return IssuedCertificate{
		Subject:     req.Subject,
		Principal:   principal,
		IdentityURI: req.Subject.URI(),
		Certificate: cert,
		ServerTLS:   serverTLS,
		ClientTLS:   clientTLS,
		TrustBundle: materials.bundle,
		ExpiresAt:   expiresAt,
	}, nil
}

func (p *LocalDevProvider) RenewCertificate(ctx context.Context, current IssuedCertificate, req IssueRequest) (IssuedCertificate, error) {
	return p.IssueCertificate(ctx, req)
}

func (p *LocalDevProvider) currentMaterials() (sharedLocalDevMaterials, error) {
	if p.dataDir == "" {
		p.mu.RLock()
		ca := p.ca
		p.mu.RUnlock()
		if ca == nil {
			return sharedLocalDevMaterials{}, fmt.Errorf("local-dev certificate authority is not initialized")
		}
		return sharedLocalDevMaterials{
			activeCA: ca,
			bundle: TrustBundle{
				TrustDomain: p.trustDomain,
				RootCAPEM:   ca.CACertPEM(),
				RootCAs:     ca.Pool(),
			},
		}, nil
	}

	materials, err := loadSharedLocalDevMaterials(p.dataDir, p.trustDomain)
	if err != nil {
		return sharedLocalDevMaterials{}, err
	}

	p.mu.Lock()
	p.ca = materials.activeCA
	p.mu.Unlock()

	return materials, nil
}

func loadSharedLocalDevMaterials(dataDir, trustDomain string) (sharedLocalDevMaterials, error) {
	issuerDir := filepath.Join(dataDir, localDevIssuerDir)
	lockFile, err := lockLocalDevIssuer(issuerDir)
	if err != nil {
		return sharedLocalDevMaterials{}, err
	}
	defer unlockLocalDevIssuer(lockFile)

	activeCA, state, err := ensureSharedIssuerStateLocked(issuerDir, time.Now().UTC())
	if err != nil {
		return sharedLocalDevMaterials{}, err
	}

	bundle, err := buildTrustBundle(trustDomain, activeCA.CACertPEM(), state.Retired)
	if err != nil {
		return sharedLocalDevMaterials{}, err
	}

	return sharedLocalDevMaterials{
		activeCA: activeCA,
		bundle:   bundle,
	}, nil
}

func ensureSharedIssuerStateLocked(issuerDir string, now time.Time) (*mtls.ClusterCA, localDevTrustState, error) {
	activeCA, err := loadOrCreateActiveCALocked(issuerDir)
	if err != nil {
		return nil, localDevTrustState{}, err
	}

	state, err := loadLocalDevTrustStateLocked(issuerDir)
	if err != nil {
		return nil, localDevTrustState{}, err
	}
	state.Retired = pruneRetiredRoots(state.Retired, now)

	if shouldRotateLocalDevCA(activeCA.Certificate(), now) {
		if activeCA.Certificate().NotAfter.After(now) {
			state.Retired = append(state.Retired, retiredLocalDevRoot{
				CertPEM:             append([]byte(nil), activeCA.CACertPEM()...),
				RetireAfterUnixNano: now.Add(localDevRetiredRootTTL).UnixNano(),
			})
		}

		rotatedCA, err := mtls.NewClusterCA()
		if err != nil {
			return nil, localDevTrustState{}, err
		}
		if err := writeActiveCALocked(issuerDir, rotatedCA); err != nil {
			return nil, localDevTrustState{}, err
		}
		activeCA = rotatedCA
	}

	if err := writeLocalDevTrustStateLocked(issuerDir, state); err != nil {
		return nil, localDevTrustState{}, err
	}

	return activeCA, state, nil
}

func loadOrCreateActiveCALocked(issuerDir string) (*mtls.ClusterCA, error) {
	certPath := filepath.Join(issuerDir, "ca.pem")
	keyPath := filepath.Join(issuerDir, "ca-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return mtls.LoadClusterCA(certPEM, keyPEM)
	case certErr == nil || keyErr == nil:
		return nil, fmt.Errorf("local-dev issuer files are incomplete in %s", issuerDir)
	case !os.IsNotExist(certErr) || !os.IsNotExist(keyErr):
		if certErr != nil {
			return nil, certErr
		}
		return nil, keyErr
	}

	ca, err := mtls.NewClusterCA()
	if err != nil {
		return nil, err
	}
	if err := writeActiveCALocked(issuerDir, ca); err != nil {
		return nil, err
	}
	return ca, nil
}

func writeActiveCALocked(issuerDir string, ca *mtls.ClusterCA) error {
	if err := os.MkdirAll(issuerDir, 0700); err != nil {
		return fmt.Errorf("create local-dev issuer dir: %w", err)
	}
	keyPEM, err := ca.CAKeyPEM()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(issuerDir, "ca.pem"), ca.CACertPEM(), 0600); err != nil {
		return fmt.Errorf("write local-dev CA cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(issuerDir, "ca-key.pem"), keyPEM, 0600); err != nil {
		return fmt.Errorf("write local-dev CA key: %w", err)
	}
	return nil
}

func buildTrustBundle(trustDomain string, activeCertPEM []byte, retired []retiredLocalDevRoot) (TrustBundle, error) {
	roots := make([][]byte, 0, 1+len(retired))
	roots = append(roots, append([]byte(nil), activeCertPEM...))
	for _, root := range retired {
		if len(root.CertPEM) == 0 {
			continue
		}
		roots = append(roots, append([]byte(nil), root.CertPEM...))
	}

	pool := x509.NewCertPool()
	rootPEM := make([]byte, 0)
	for _, pemData := range roots {
		rootPEM = append(rootPEM, pemData...)
		if len(rootPEM) == 0 || rootPEM[len(rootPEM)-1] != '\n' {
			rootPEM = append(rootPEM, '\n')
		}
		if !pool.AppendCertsFromPEM(pemData) {
			return TrustBundle{}, fmt.Errorf("parse local-dev trust bundle")
		}
	}

	return TrustBundle{
		TrustDomain: trustDomain,
		RootCAPEM:   rootPEM,
		RootCAs:     pool,
	}, nil
}

func loadLocalDevTrustStateLocked(issuerDir string) (localDevTrustState, error) {
	path := filepath.Join(issuerDir, localDevBundleStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return localDevTrustState{}, nil
		}
		return localDevTrustState{}, err
	}

	var state localDevTrustState
	if err := json.Unmarshal(data, &state); err != nil {
		return localDevTrustState{}, fmt.Errorf("parse local-dev trust state: %w", err)
	}
	return state, nil
}

func writeLocalDevTrustStateLocked(issuerDir string, state localDevTrustState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal local-dev trust state: %w", err)
	}
	return os.WriteFile(filepath.Join(issuerDir, localDevBundleStateFile), data, 0600)
}

func pruneRetiredRoots(roots []retiredLocalDevRoot, now time.Time) []retiredLocalDevRoot {
	if len(roots) == 0 {
		return nil
	}

	filtered := make([]retiredLocalDevRoot, 0, len(roots))
	for _, root := range roots {
		if len(root.CertPEM) == 0 {
			continue
		}
		retireAfter := time.Unix(0, root.RetireAfterUnixNano).UTC()
		if retireAfter.IsZero() || retireAfter.After(now) {
			filtered = append(filtered, root)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func shouldRotateLocalDevCA(cert *x509.Certificate, now time.Time) bool {
	if cert == nil {
		return true
	}
	return cert.NotAfter.Sub(now) <= localDevCARotationWindow
}

func lockLocalDevIssuer(issuerDir string) (*os.File, error) {
	if err := os.MkdirAll(issuerDir, 0700); err != nil {
		return nil, fmt.Errorf("create local-dev issuer dir: %w", err)
	}
	lockPath := filepath.Join(issuerDir, ".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open local-dev issuer lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire local-dev issuer lock: %w", err)
	}
	return file, nil
}

func unlockLocalDevIssuer(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func requestedTTL(value time.Duration) time.Duration {
	switch {
	case value <= 0:
		return defaultLocalDevTTL
	case value > localDevMaxLeafTTL:
		return localDevMaxLeafTTL
	default:
		return value
	}
}

func uniqueIPs(values []net.IP) []net.IP {
	seen := make(map[string]struct{}, len(values))
	result := make([]net.IP, 0, len(values))
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		key := value.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}
