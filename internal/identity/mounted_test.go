package identity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/mtls"
)

func TestMountedProviderLoadsMatchingSubject(t *testing.T) {
	dir := t.TempDir()
	subject := Subject{
		TrustDomain: "spiffe.demo.internal",
		Fabric:      "demo",
		Kind:        SubjectKindConductor,
		Name:        "conductor",
	}
	writeMountedIdentity(t, dir, subject, "conductor")

	provider, err := NewMountedProvider(
		subject.TrustDomain,
		filepath.Join(dir, "cert.pem"),
		filepath.Join(dir, "key.pem"),
		filepath.Join(dir, "bundle.pem"),
	)
	if err != nil {
		t.Fatalf("NewMountedProvider: %v", err)
	}

	issued, err := provider.IssueCertificate(context.Background(), IssueRequest{
		Subject:   subject,
		Principal: "conductor",
	})
	if err != nil {
		t.Fatalf("IssueCertificate: %v", err)
	}
	if issued.Subject.URI() != subject.URI() {
		t.Fatalf("subject URI = %q, want %q", issued.Subject.URI(), subject.URI())
	}
	if issued.TrustBundle.TrustDomain != subject.TrustDomain {
		t.Fatalf("trust domain = %q, want %q", issued.TrustBundle.TrustDomain, subject.TrustDomain)
	}
}

func TestMountedProviderRejectsMismatchedSubject(t *testing.T) {
	dir := t.TempDir()
	subject := Subject{
		TrustDomain: "spiffe.demo.internal",
		Fabric:      "demo",
		Kind:        SubjectKindConductor,
		Name:        "conductor",
	}
	writeMountedIdentity(t, dir, subject, "conductor")

	provider, err := NewMountedProvider(
		subject.TrustDomain,
		filepath.Join(dir, "cert.pem"),
		filepath.Join(dir, "key.pem"),
		filepath.Join(dir, "bundle.pem"),
	)
	if err != nil {
		t.Fatalf("NewMountedProvider: %v", err)
	}

	_, err = provider.IssueCertificate(context.Background(), IssueRequest{
		Subject: Subject{
			TrustDomain: subject.TrustDomain,
			Fabric:      subject.Fabric,
			Kind:        SubjectKindNode,
			Name:        "node-a",
			NodeID:      "node-a",
		},
		Principal: "node-a",
	})
	if err == nil {
		t.Fatal("expected subject mismatch error")
	}
}

func writeMountedIdentity(t *testing.T, dir string, subject Subject, principal string) {
	t.Helper()

	ca, err := mtls.NewClusterCA()
	if err != nil {
		t.Fatalf("NewClusterCA: %v", err)
	}
	cert, err := ca.IssueCertWithOptions(principal, mtls.IssueCertOptions{
		IdentityURI: subject.URI(),
	})
	if err != nil {
		t.Fatalf("IssueCertWithOptions: %v", err)
	}
	if err := mtls.WriteAgentCredentials(dir, cert, ca.CACertPEM()); err != nil {
		t.Fatalf("WriteAgentCredentials: %v", err)
	}

	if err := os.Rename(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "bundle.pem")); err != nil {
		t.Fatalf("Rename bundle: %v", err)
	}
}
