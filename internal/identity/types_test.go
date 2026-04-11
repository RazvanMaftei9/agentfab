package identity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubjectURIForAgentInstance(t *testing.T) {
	subject := Subject{
		TrustDomain: "agentfab.local",
		Fabric:      "test-fabric",
		Kind:        SubjectKindAgentInstance,
		Name:        "developer-instance",
		NodeID:      "node-a",
		Profile:     "developer",
		InstanceID:  "node-a/developer/1",
	}

	uri := subject.URI()
	if !strings.HasPrefix(uri, "spiffe://agentfab.local/") {
		t.Fatalf("URI prefix = %q", uri)
	}
	if !strings.Contains(uri, "/fabric/test-fabric/") {
		t.Fatalf("URI missing fabric segment: %q", uri)
	}
	if !strings.Contains(uri, "/agent/developer/") {
		t.Fatalf("URI missing agent segment: %q", uri)
	}
}

func TestLocalDevProviderIssuesCertificate(t *testing.T) {
	provider, err := NewLocalDevProvider("agentfab.local")
	if err != nil {
		t.Fatalf("NewLocalDevProvider: %v", err)
	}

	issued, err := provider.IssueCertificate(context.Background(), IssueRequest{
		Subject: Subject{
			TrustDomain: "agentfab.local",
			Fabric:      "test-fabric",
			Kind:        SubjectKindConductor,
			Name:        "conductor",
		},
		Principal: "conductor",
	})
	if err != nil {
		t.Fatalf("IssueCertificate: %v", err)
	}

	if issued.IdentityURI == "" {
		t.Fatal("expected identity URI")
	}
	if issued.ServerTLS == nil {
		t.Fatal("expected server TLS config")
	}
	if issued.ClientTLS == nil {
		t.Fatal("expected client TLS config")
	}
	if len(issued.TrustBundle.RootCAPEM) == 0 {
		t.Fatal("expected root CA PEM")
	}
}

func TestParseSubjectURIForAgentInstance(t *testing.T) {
	subject, err := ParseSubjectURI("spiffe://agentfab.local/fabric/test-fabric/node/node-a/agent/developer/instance/node-a-developer-1")
	if err != nil {
		t.Fatalf("ParseSubjectURI: %v", err)
	}

	if subject.Kind != SubjectKindAgentInstance {
		t.Fatalf("Kind = %q, want %q", subject.Kind, SubjectKindAgentInstance)
	}
	if subject.NodeID != "node-a" {
		t.Fatalf("NodeID = %q, want %q", subject.NodeID, "node-a")
	}
	if subject.Profile != "developer" {
		t.Fatalf("Profile = %q, want %q", subject.Profile, "developer")
	}
	if subject.InstanceID != "node-a-developer-1" {
		t.Fatalf("InstanceID = %q, want %q", subject.InstanceID, "node-a-developer-1")
	}
}

func TestSharedLocalDevProviderReusesCA(t *testing.T) {
	dataDir := t.TempDir()

	first, err := NewSharedLocalDevProvider(dataDir, "agentfab.local")
	if err != nil {
		t.Fatalf("NewSharedLocalDevProvider (first): %v", err)
	}
	second, err := NewSharedLocalDevProvider(dataDir, "agentfab.local")
	if err != nil {
		t.Fatalf("NewSharedLocalDevProvider (second): %v", err)
	}

	firstBundle, err := first.TrustBundle(context.Background())
	if err != nil {
		t.Fatalf("TrustBundle (first): %v", err)
	}
	secondBundle, err := second.TrustBundle(context.Background())
	if err != nil {
		t.Fatalf("TrustBundle (second): %v", err)
	}

	if string(firstBundle.RootCAPEM) != string(secondBundle.RootCAPEM) {
		t.Fatal("expected shared local-dev providers to reuse the same CA")
	}

	certPath := filepath.Join(dataDir, localDevIssuerDir, "ca.pem")
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("expected shared CA cert on disk: %v", err)
	}
}
