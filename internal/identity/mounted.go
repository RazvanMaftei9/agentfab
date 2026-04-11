package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/razvanmaftei/agentfab/internal/mtls"
)

type MountedProvider struct {
	trustDomain string
	certFile    string
	keyFile     string
	bundleFile  string
}

func NewMountedProvider(trustDomain, certFile, keyFile, bundleFile string) (*MountedProvider, error) {
	if certFile == "" {
		return nil, fmt.Errorf("mounted identity cert file is required")
	}
	if keyFile == "" {
		return nil, fmt.Errorf("mounted identity key file is required")
	}
	if bundleFile == "" {
		return nil, fmt.Errorf("mounted identity bundle file is required")
	}
	return &MountedProvider{
		trustDomain: trustDomain,
		certFile:    certFile,
		keyFile:     keyFile,
		bundleFile:  bundleFile,
	}, nil
}

func (p *MountedProvider) Name() string {
	return "mounted"
}

func (p *MountedProvider) SupportsArbitrarySubjects() bool {
	return false
}

func (p *MountedProvider) Files() (certFile, keyFile, bundleFile string) {
	return p.certFile, p.keyFile, p.bundleFile
}

func (p *MountedProvider) TrustBundle(_ context.Context) (TrustBundle, error) {
	bundlePEM, err := os.ReadFile(p.bundleFile)
	if err != nil {
		return TrustBundle{}, fmt.Errorf("read mounted trust bundle: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundlePEM) {
		return TrustBundle{}, fmt.Errorf("parse mounted trust bundle")
	}

	return TrustBundle{
		TrustDomain: p.trustDomain,
		RootCAPEM:   bundlePEM,
		RootCAs:     pool,
	}, nil
}

func (p *MountedProvider) IssueCertificate(ctx context.Context, req IssueRequest) (IssuedCertificate, error) {
	return p.loadIssuedCertificate(ctx, req)
}

func (p *MountedProvider) RenewCertificate(ctx context.Context, current IssuedCertificate, req IssueRequest) (IssuedCertificate, error) {
	return p.loadIssuedCertificate(ctx, req)
}

func (p *MountedProvider) loadIssuedCertificate(ctx context.Context, req IssueRequest) (IssuedCertificate, error) {
	if err := req.Subject.Validate(); err != nil {
		return IssuedCertificate{}, err
	}

	bundle, err := p.TrustBundle(ctx)
	if err != nil {
		return IssuedCertificate{}, err
	}

	cert, leaf, err := p.loadLeafCertificate()
	if err != nil {
		return IssuedCertificate{}, err
	}

	subject, err := SubjectFromCertificate(leaf)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("read mounted certificate subject: %w", err)
	}
	if subject.URI() != req.Subject.URI() {
		return IssuedCertificate{}, fmt.Errorf("mounted identity subject %q does not match requested subject %q", subject.URI(), req.Subject.URI())
	}

	serverTLS := mtls.ServerTLSConfig(cert, bundle.RootCAs)
	clientTLS := mtls.ClientTLSConfig(cert, bundle.RootCAs)

	return IssuedCertificate{
		Subject:     subject,
		Principal:   leaf.Subject.CommonName,
		IdentityURI: subject.URI(),
		Certificate: cert,
		ServerTLS:   serverTLS,
		ClientTLS:   clientTLS,
		TrustBundle: bundle,
		ExpiresAt:   leaf.NotAfter,
	}, nil
}

func (p *MountedProvider) loadLeafCertificate() (tls.Certificate, *x509.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(p.certFile, p.keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load mounted certificate key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, nil, fmt.Errorf("mounted certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse mounted leaf certificate: %w", err)
	}
	return cert, leaf, nil
}
