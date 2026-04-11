package identity

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestManagedCertificateRenewsBeforeExpiry(t *testing.T) {
	provider, err := NewLocalDevProvider("agentfab.local")
	if err != nil {
		t.Fatalf("NewLocalDevProvider: %v", err)
	}

	managed, err := NewManagedCertificate(context.Background(), provider, IssueRequest{
		Subject: Subject{
			TrustDomain: "agentfab.local",
			Fabric:      "test-fabric",
			Kind:        SubjectKindConductor,
			Name:        "conductor",
		},
		Principal:    "conductor",
		RequestedTTL: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewManagedCertificate: %v", err)
	}
	defer managed.Close()

	initial := managed.Current()

	select {
	case <-managed.Updates():
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for certificate renewal")
	}

	renewed := managed.Current()
	if renewed.ExpiresAt.Before(initial.ExpiresAt) {
		t.Fatalf("renewed expiry %v before initial expiry %v", renewed.ExpiresAt, initial.ExpiresAt)
	}
	if bytes.Equal(initial.Certificate.Certificate[0], renewed.Certificate.Certificate[0]) {
		t.Fatal("expected renewed certificate material")
	}
}
