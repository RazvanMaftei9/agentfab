package controlplanesvc

import (
	"context"
	"testing"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/identity"
)

func TestServicePersistsNodeAttestationAcrossInstances(t *testing.T) {
	ctx := context.Background()
	store := controlplane.NewMemoryStore("test-fabric")
	authority := identity.NewLocalDevJoinTokenAuthority(t.TempDir(), "agentfab.local")
	token, err := authority.IssueNodeToken(ctx, identity.NodeTokenRequest{
		Fabric:    "test-fabric",
		NodeID:    "node-a",
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  true,
	})
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	serviceA := New(Config{
		Store:    store,
		Fabric:   "test-fabric",
		Attestor: authority,
	})
	subjectURI := identity.Subject{
		TrustDomain: "agentfab.local",
		Fabric:      "test-fabric",
		Kind:        identity.SubjectKindNode,
		Name:        "node-a",
		NodeID:      "node-a",
	}.URI()

	_, err = serviceA.AttestNode(ctx, subjectURI, &pb.AttestNodeRequest{
		Type:  identity.NodeJoinTokenAttestationType,
		Token: token.Value,
	})
	if err != nil {
		t.Fatalf("AttestNode: %v", err)
	}

	serviceB := New(Config{
		Store:  store,
		Fabric: "test-fabric",
	})
	err = serviceB.RegisterNode(ctx, subjectURI, &pb.RegisterNodeRequest{
		NodeId:       "node-a",
		Address:      "127.0.0.1",
		State:        string(controlplane.NodeStateReady),
		BundleDigest: "bundle-1",
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	node, ok, err := store.GetNode(ctx, "node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !ok {
		t.Fatal("expected node to be persisted")
	}
	if node.ID != "node-a" {
		t.Fatalf("node ID = %q, want node-a", node.ID)
	}
}

func TestServiceAllowsNodeSubjectToManageHostedInstanceMembership(t *testing.T) {
	ctx := context.Background()
	store := controlplane.NewMemoryStore("test-fabric")
	authority := identity.NewLocalDevJoinTokenAuthority(t.TempDir(), "agentfab.local")
	token, err := authority.IssueNodeToken(ctx, identity.NodeTokenRequest{
		Fabric:    "test-fabric",
		NodeID:    "node-a",
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  true,
	})
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	service := New(Config{
		Store:                  store,
		Fabric:                 "test-fabric",
		ExpectedProfileDigests: map[string]string{"developer": "profile-digest-1"},
		Attestor:               authority,
	})
	subjectURI := identity.Subject{
		TrustDomain: "agentfab.local",
		Fabric:      "test-fabric",
		Kind:        identity.SubjectKindNode,
		Name:        "node-a",
		NodeID:      "node-a",
	}.URI()

	_, err = service.AttestNode(ctx, subjectURI, &pb.AttestNodeRequest{
		Type:  identity.NodeJoinTokenAttestationType,
		Token: token.Value,
	})
	if err != nil {
		t.Fatalf("AttestNode: %v", err)
	}

	err = service.RegisterInstance(ctx, subjectURI, &pb.RegisterInstanceRequest{
		InstanceId:              "node-a/developer",
		Profile:                 "developer",
		ProfileDigest:           "profile-digest-1",
		NodeId:                  "node-a",
		EndpointAddress:         "127.0.0.1:50051",
		StartedAtUnixNano:       time.Now().UTC().UnixNano(),
		LastHeartbeatAtUnixNano: time.Now().UTC().UnixNano(),
		State:                   string(controlplane.InstanceStateReady),
	})
	if err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}

	heartbeatAt := time.Now().UTC().Add(5 * time.Second)
	err = service.HeartbeatInstance(ctx, subjectURI, &pb.HeartbeatInstanceRequest{
		InstanceId: "node-a/developer",
		AtUnixNano: heartbeatAt.UnixNano(),
	})
	if err != nil {
		t.Fatalf("HeartbeatInstance: %v", err)
	}

	instance, ok, err := store.GetInstance(ctx, "node-a/developer")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if !ok {
		t.Fatal("expected instance to be persisted")
	}
	if instance.NodeID != "node-a" {
		t.Fatalf("instance node = %q, want node-a", instance.NodeID)
	}
	if !instance.LastHeartbeatAt.Equal(heartbeatAt) {
		t.Fatalf("instance heartbeat = %v, want %v", instance.LastHeartbeatAt, heartbeatAt)
	}
}
