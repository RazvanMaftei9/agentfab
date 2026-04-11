package mtls

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
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	clusterCALifetime = 365 * 24 * time.Hour
	defaultLeafTTL    = 24 * time.Hour
)

type ClusterCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	tlsCert tls.Certificate
	pool    *x509.CertPool
}

type IssueCertOptions struct {
	DNSNames    []string
	IPAddresses []net.IP
	IdentityURI string
	TTL         time.Duration
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
		NotAfter:              time.Now().Add(clusterCALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
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

func LoadClusterCA(certPEM, keyPEM []byte) (*ClusterCA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("parse CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("parse CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA key pair: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return &ClusterCA{
		cert:    cert,
		key:     key,
		certPEM: certPEM,
		tlsCert: tlsCert,
		pool:    pool,
	}, nil
}

func (ca *ClusterCA) IssueCert(name string) (tls.Certificate, error) {
	return ca.IssueCertWithOptions(name, IssueCertOptions{})
}

func (ca *ClusterCA) IssueCertWithOptions(name string, options IssueCertOptions) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key for %q: %w", name, err)
	}
	ttl := options.TTL
	if ttl <= 0 {
		ttl = defaultLeafTTL
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
		NotAfter:    time.Now().Add(ttl),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    append([]string{"localhost", name}, options.DNSNames...),
		IPAddresses: append([]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, options.IPAddresses...),
	}
	if options.IdentityURI != "" {
		identityURI, err := url.Parse(options.IdentityURI)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("parse identity URI for %q: %w", name, err)
		}
		tmpl.URIs = []*url.URL{identityURI}
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
	cert.Certificate = append(cert.Certificate, ca.cert.Raw)

	return cert, nil
}

func (ca *ClusterCA) Pool() *x509.CertPool { return ca.pool }

func (ca *ClusterCA) Certificate() *x509.Certificate {
	return ca.cert
}

func (ca *ClusterCA) CACertPEM() []byte { return ca.certPEM }

func (ca *ClusterCA) CAKeyPEM() ([]byte, error) {
	keyDER, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

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

type TLSMaterialSource interface {
	CurrentTLSMaterial() (tls.Certificate, *x509.CertPool, error)
}

func DynamicServerTLSConfig(source TLSMaterialSource) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool, err := source.CurrentTLSMaterial()
			if err != nil {
				return nil, err
			}
			return ServerTLSConfig(cert, pool), nil
		},
	}
}

func DynamicClientTLSConfig(source TLSMaterialSource) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, _, err := source.CurrentTLSMaterial()
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
		VerifyConnection: func(state tls.ConnectionState) error {
			_, pool, err := source.CurrentTLSMaterial()
			if err != nil {
				return err
			}
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("missing peer certificate")
			}

			intermediates := x509.NewCertPool()
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}

			verifyOptions := x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intermediates,
				CurrentTime:   time.Now(),
			}
			if state.ServerName != "" {
				verifyOptions.DNSName = state.ServerName
			}

			_, err = state.PeerCertificates[0].Verify(verifyOptions)
			return err
		},
	}
}

func WriteAgentCredentials(dir string, agentCert tls.Certificate, caCertPEM []byte) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create TLS dir: %w", err)
	}
	lockFile, err := lockTLSDir(dir, true)
	if err != nil {
		return err
	}
	defer unlockTLSDir(lockFile)

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: agentCert.Certificate[0]})
	if err := writeLockedFile(filepath.Join(dir, "cert.pem"), leafPEM); err != nil {
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
	if err := writeLockedFile(filepath.Join(dir, "key.pem"), keyPEM); err != nil {
		return fmt.Errorf("write key.pem: %w", err)
	}

	if err := writeLockedFile(filepath.Join(dir, "ca.pem"), caCertPEM); err != nil {
		return fmt.Errorf("write ca.pem: %w", err)
	}

	return nil
}

func LoadTLSCredentials(dir string) (serverTLS, clientTLS *tls.Config, err error) {
	cert, pool, err := loadTLSMaterial(dir)
	if err != nil {
		return nil, nil, err
	}
	return ServerTLSConfig(cert, pool), ClientTLSConfig(cert, pool), nil
}

func LoadDynamicTLSCredentials(dir string) (serverTLS, clientTLS *tls.Config, err error) {
	source := &fileTLSMaterialSource{dir: dir}
	if _, _, err := source.CurrentTLSMaterial(); err != nil {
		return nil, nil, err
	}
	return DynamicServerTLSConfig(source), DynamicClientTLSConfig(source), nil
}

func LoadDynamicTLSCredentialsFromFiles(certFile, keyFile, caFile string) (serverTLS, clientTLS *tls.Config, err error) {
	source := &explicitFileTLSMaterialSource{
		certFile: certFile,
		keyFile:  keyFile,
		caFile:   caFile,
	}
	if _, _, err := source.CurrentTLSMaterial(); err != nil {
		return nil, nil, err
	}
	return DynamicServerTLSConfig(source), DynamicClientTLSConfig(source), nil
}

func loadTLSMaterial(dir string) (tls.Certificate, *x509.CertPool, error) {
	lockFile, err := lockTLSDir(dir, false)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	defer unlockTLSDir(lockFile)

	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	caFile := filepath.Join(dir, "ca.pem")

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load key pair: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("read ca.pem: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("failed to parse ca.pem")
	}

	return cert, pool, nil
}

func AgentTLSDir(dataDir, agentName string) string {
	return filepath.Join(dataDir, "agents", agentName, "tls")
}

type fileTLSMaterialSource struct {
	dir string
}

func (s *fileTLSMaterialSource) CurrentTLSMaterial() (tls.Certificate, *x509.CertPool, error) {
	return loadTLSMaterial(s.dir)
}

type explicitFileTLSMaterialSource struct {
	certFile string
	keyFile  string
	caFile   string
}

func (s *explicitFileTLSMaterialSource) CurrentTLSMaterial() (tls.Certificate, *x509.CertPool, error) {
	return loadTLSMaterialFromFiles(s.certFile, s.keyFile, s.caFile)
}

func loadTLSMaterialFromFiles(certFile, keyFile, caFile string) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load key pair: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("failed to parse CA file")
	}

	return cert, pool, nil
}

func lockTLSDir(dir string, exclusive bool) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create TLS dir: %w", err)
	}
	lockPath := filepath.Join(dir, ".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open TLS lock: %w", err)
	}

	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(file.Fd()), mode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire TLS lock: %w", err)
	}
	return file, nil
}

func unlockTLSDir(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func writeLockedFile(path string, data []byte) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(0600); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
