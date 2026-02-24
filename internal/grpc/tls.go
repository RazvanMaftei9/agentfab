package grpc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ClusterCA is an ephemeral in-memory CA that issues per-agent certificates.
type ClusterCA struct {
	cert    *x509.Certificate // parsed CA certificate
	key     *ecdsa.PrivateKey // CA private key (memory-only)
	certPEM []byte            // PEM-encoded CA certificate (shared with agents)
	tlsCert tls.Certificate   // CA cert+key as tls.Certificate (for conductor's own connections)
	pool    *x509.CertPool   // trust pool containing the CA cert
}

func NewClusterCA() (*ClusterCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial number: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"agentfab"},
			CommonName:   "agentfab-cluster-ca",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0, // leaf certs only
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA key pair: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return &ClusterCA{
		cert:    caCert,
		key:     key,
		certPEM: certPEM,
		tlsCert: tlsCert,
		pool:    pool,
	}, nil
}

// IssueCert creates a CA-signed certificate for the named participant.
func (ca *ClusterCA) IssueCert(name string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key for %q: %w", name, err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serial number: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"agentfab"},
			CommonName:   name,
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"localhost", name},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sign certificate for %q: %w", name, err)
	}

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key for %q: %w", name, err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(leafPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse key pair for %q: %w", name, err)
	}
	// Append CA cert to chain so the peer can verify.
	cert.Certificate = append(cert.Certificate, ca.cert.Raw)

	return cert, nil
}

func (ca *ClusterCA) Pool() *x509.CertPool { return ca.pool }
func (ca *ClusterCA) CACertPEM() []byte     { return ca.certPEM }

func ServerTLSConfig(cert tls.Certificate, clientCA *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCA,
		MinVersion:   tls.VersionTLS13,
	}
}

func ClientTLSConfig(cert tls.Certificate, serverCA *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      serverCA,
		MinVersion:   tls.VersionTLS13,
	}
}

// WriteAgentCredentials writes an agent's cert+key and the CA cert to dir.
func WriteAgentCredentials(dir string, agentCert tls.Certificate, caCertPEM []byte) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create TLS dir: %w", err)
	}

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: agentCert.Certificate[0]})
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), leafPEM, 0600); err != nil {
		return fmt.Errorf("write cert.pem: %w", err)
	}

	ecKey, ok := agentCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("unexpected private key type %T", agentCert.PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0600); err != nil {
		return fmt.Errorf("write key.pem: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), caCertPEM, 0600); err != nil {
		return fmt.Errorf("write ca.pem: %w", err)
	}

	return nil
}

// LoadTLSCredentials loads cert, key, and CA from dir, returning server and client TLS configs.
func LoadTLSCredentials(dir string) (serverTLS, clientTLS *tls.Config, err error) {
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	caFile := filepath.Join(dir, "ca.pem")

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load key pair: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read ca.pem: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("failed to parse ca.pem")
	}

	return ServerTLSConfig(cert, pool), ClientTLSConfig(cert, pool), nil
}

func AgentTLSDir(dataDir, agentName string) string {
	return filepath.Join(dataDir, "agents", agentName, "tls")
}
