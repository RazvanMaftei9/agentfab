package grpc

import (
	"crypto/tls"
	"crypto/x509"

	"github.com/razvanmaftei/agentfab/internal/mtls"
)

type ClusterCA = mtls.ClusterCA
type IssueCertOptions = mtls.IssueCertOptions

func NewClusterCA() (*ClusterCA, error) {
	return mtls.NewClusterCA()
}

func ServerTLSConfig(cert tls.Certificate, clientCA *x509.CertPool) *tls.Config {
	return mtls.ServerTLSConfig(cert, clientCA)
}

func ClientTLSConfig(cert tls.Certificate, serverCA *x509.CertPool) *tls.Config {
	return mtls.ClientTLSConfig(cert, serverCA)
}

func WriteAgentCredentials(dir string, agentCert tls.Certificate, caCertPEM []byte) error {
	return mtls.WriteAgentCredentials(dir, agentCert, caCertPEM)
}

func LoadDynamicTLSCredentials(dir string) (serverTLS, clientTLS *tls.Config, err error) {
	return mtls.LoadDynamicTLSCredentials(dir)
}
