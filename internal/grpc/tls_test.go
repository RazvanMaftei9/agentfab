package grpc

import (
	"crypto/tls"
	"testing"
)

func TestLoadDynamicTLSCredentialsReloadsUpdatedFiles(t *testing.T) {
	dir := t.TempDir()

	ca, err := NewClusterCA()
	if err != nil {
		t.Fatalf("NewClusterCA: %v", err)
	}

	initial, err := ca.IssueCert("agent")
	if err != nil {
		t.Fatalf("IssueCert initial: %v", err)
	}
	if err := WriteAgentCredentials(dir, initial, ca.CACertPEM()); err != nil {
		t.Fatalf("WriteAgentCredentials initial: %v", err)
	}

	serverTLS, clientTLS, err := LoadDynamicTLSCredentials(dir)
	if err != nil {
		t.Fatalf("LoadDynamicTLSCredentials: %v", err)
	}

	renewed, err := ca.IssueCert("agent")
	if err != nil {
		t.Fatalf("IssueCert renewed: %v", err)
	}
	if err := WriteAgentCredentials(dir, renewed, ca.CACertPEM()); err != nil {
		t.Fatalf("WriteAgentCredentials renewed: %v", err)
	}

	clientCert, err := clientTLS.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if len(clientCert.Certificate) == 0 || len(renewed.Certificate) == 0 {
		t.Fatal("expected certificate chain")
	}
	if string(clientCert.Certificate[0]) != string(renewed.Certificate[0]) {
		t.Fatal("expected client TLS config to reload renewed certificate")
	}

	serverConfig, err := serverTLS.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetConfigForClient: %v", err)
	}
	if len(serverConfig.Certificates) == 0 || len(serverConfig.Certificates[0].Certificate) == 0 {
		t.Fatal("expected server certificate chain")
	}
	if string(serverConfig.Certificates[0].Certificate[0]) != string(renewed.Certificate[0]) {
		t.Fatal("expected server TLS config to reload renewed certificate")
	}
}
