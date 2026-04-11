package identity

import (
	"context"
	"testing"
	"time"
)

func TestLocalDevJoinTokenAuthorityAttestsBoundNode(t *testing.T) {
	authority := NewLocalDevJoinTokenAuthority(t.TempDir(), "agentfab.local")
	token, err := authority.IssueNodeToken(context.Background(), NodeTokenRequest{
		Fabric:    "test-fabric",
		NodeID:    "node-a",
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  true,
		ExpectedMeasurements: map[string]string{
			"bundle_digest": "bundle-a",
		},
	})
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	attestedNode, err := authority.AttestNode(context.Background(), NodeAttestation{
		Type:  NodeJoinTokenAttestationType,
		Token: token.Value,
		Claims: map[string]string{
			"node_id": "node-a",
			"fabric":  "test-fabric",
		},
		Measurements: map[string]string{
			"bundle_digest": "bundle-a",
		},
	})
	if err != nil {
		t.Fatalf("AttestNode: %v", err)
	}

	if attestedNode.NodeID != "node-a" {
		t.Fatalf("NodeID = %q, want %q", attestedNode.NodeID, "node-a")
	}
	if attestedNode.TrustDomain != "agentfab.local" {
		t.Fatalf("TrustDomain = %q, want %q", attestedNode.TrustDomain, "agentfab.local")
	}
	if attestedNode.Measurements["bundle_digest"] != "bundle-a" {
		t.Fatalf("bundle_digest = %q, want %q", attestedNode.Measurements["bundle_digest"], "bundle-a")
	}
}

func TestLocalDevJoinTokenAuthorityRejectsWrongNode(t *testing.T) {
	authority := NewLocalDevJoinTokenAuthority(t.TempDir(), "agentfab.local")
	token, err := authority.IssueNodeToken(context.Background(), NodeTokenRequest{
		Fabric:    "test-fabric",
		NodeID:    "node-a",
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  true,
	})
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	_, err = authority.AttestNode(context.Background(), NodeAttestation{
		Type:  NodeJoinTokenAttestationType,
		Token: token.Value,
		Claims: map[string]string{
			"node_id": "node-b",
			"fabric":  "test-fabric",
		},
	})
	if err == nil {
		t.Fatal("expected attestation failure")
	}
}

func TestLocalDevJoinTokenAuthorityRejectsSecondUseForOneTimeToken(t *testing.T) {
	authority := NewLocalDevJoinTokenAuthority(t.TempDir(), "agentfab.local")
	token, err := authority.IssueNodeToken(context.Background(), NodeTokenRequest{
		Fabric:    "test-fabric",
		NodeID:    "node-a",
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  false,
	})
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	request := NodeAttestation{
		Type:  NodeJoinTokenAttestationType,
		Token: token.Value,
		Claims: map[string]string{
			"node_id": "node-a",
			"fabric":  "test-fabric",
		},
	}
	if _, err := authority.AttestNode(context.Background(), request); err != nil {
		t.Fatalf("first AttestNode: %v", err)
	}
	if _, err := authority.AttestNode(context.Background(), request); err == nil {
		t.Fatal("expected second attestation to fail")
	}
}

func TestLocalDevJoinTokenAuthorityRejectsWrongMeasurement(t *testing.T) {
	authority := NewLocalDevJoinTokenAuthority(t.TempDir(), "agentfab.local")
	token, err := authority.IssueNodeToken(context.Background(), NodeTokenRequest{
		Fabric:    "test-fabric",
		NodeID:    "node-a",
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  true,
		ExpectedMeasurements: map[string]string{
			"binary_sha256": "expected-binary",
		},
	})
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	_, err = authority.AttestNode(context.Background(), NodeAttestation{
		Type:  NodeJoinTokenAttestationType,
		Token: token.Value,
		Claims: map[string]string{
			"node_id": "node-a",
			"fabric":  "test-fabric",
		},
		Measurements: map[string]string{
			"binary_sha256": "different-binary",
		},
	})
	if err == nil {
		t.Fatal("expected attestation failure")
	}
}
