package identity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedLocalDevProviderReplacesExpiredCA(t *testing.T) {
	dataDir := t.TempDir()
	expiredPEM := writeLocalDevCA(t, dataDir, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))

	provider, err := NewSharedLocalDevProvider(dataDir, "agentfab.local")
	if err != nil {
		t.Fatalf("NewSharedLocalDevProvider: %v", err)
	}

	bundle, err := provider.TrustBundle(context.Background())
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	if bytes.Equal(bundle.RootCAPEM, expiredPEM) {
		t.Fatal("expected expired CA to be replaced")
	}

	certBlock, _ := pem.Decode(bundle.RootCAPEM)
	if certBlock == nil {
		t.Fatal("decode regenerated CA")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if !cert.NotAfter.After(time.Now()) {
		t.Fatalf("expected regenerated CA to be valid, got expiry %v", cert.NotAfter)
	}
}

func TestSharedLocalDevProviderRotatesNearExpiryCAWithDualRootBundle(t *testing.T) {
	dataDir := t.TempDir()
	previousPEM := writeLocalDevCA(t, dataDir, time.Now().Add(-24*time.Hour), time.Now().Add(12*time.Hour))

	provider, err := NewSharedLocalDevProvider(dataDir, "agentfab.local")
	if err != nil {
		t.Fatalf("NewSharedLocalDevProvider: %v", err)
	}

	bundle, err := provider.TrustBundle(context.Background())
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	if !bytes.Contains(bundle.RootCAPEM, previousPEM) {
		t.Fatal("expected rotated trust bundle to retain previous root during overlap")
	}
	if countCertificates(bundle.RootCAPEM) != 2 {
		t.Fatalf("expected dual-root bundle after rotation, got %d roots", countCertificates(bundle.RootCAPEM))
	}

	issued, err := provider.IssueCertificate(context.Background(), IssueRequest{
		Subject: Subject{
			TrustDomain: "agentfab.local",
			Fabric:      "test-fabric",
			Kind:        SubjectKindNode,
			Name:        "node-a",
			NodeID:      "node-a",
		},
		Principal: "node-a",
	})
	if err != nil {
		t.Fatalf("IssueCertificate: %v", err)
	}
	if !bytes.Contains(issued.TrustBundle.RootCAPEM, previousPEM) {
		t.Fatal("expected issued certificate bundle to include previous root during rollover")
	}
}

func writeLocalDevCA(t *testing.T, dataDir string, notBefore, notAfter time.Time) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial number: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"agentfab"},
			CommonName:   "expired-agentfab-cluster-ca",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	issuerDir := filepath.Join(dataDir, localDevIssuerDir)
	if err := os.MkdirAll(issuerDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(issuerDir, "ca.pem"), certPEM, 0600); err != nil {
		t.Fatalf("WriteFile cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(issuerDir, "ca-key.pem"), keyPEM, 0600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}

	return certPEM
}

func countCertificates(pemData []byte) int {
	count := 0
	for len(pemData) > 0 {
		block, rest := pem.Decode(pemData)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			count++
		}
		pemData = rest
	}
	return count
}
