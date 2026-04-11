package config

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestComputeBundleFingerprintChangesWithSpecialKnowledge(t *testing.T) {
	dir := t.TempDir()
	knowledgePath := filepath.Join(dir, "developer.md")
	if err := os.WriteFile(knowledgePath, []byte("original knowledge"), 0644); err != nil {
		t.Fatalf("write special knowledge: %v", err)
	}

	td := &FabricDef{
		Fabric:    FabricMeta{Name: "demo", Version: 1},
		AgentsDir: dir,
		Agents: []runtime.AgentDefinition{
			{
				Name:                 "developer",
				Model:                "test-model",
				SpecialKnowledgeFile: "developer.md",
			},
		},
	}

	first, err := ComputeBundleFingerprint(td)
	if err != nil {
		t.Fatalf("ComputeBundleFingerprint first: %v", err)
	}

	if err := os.WriteFile(knowledgePath, []byte("updated knowledge"), 0644); err != nil {
		t.Fatalf("rewrite special knowledge: %v", err)
	}

	second, err := ComputeBundleFingerprint(td)
	if err != nil {
		t.Fatalf("ComputeBundleFingerprint second: %v", err)
	}

	if first.BundleDigest == second.BundleDigest {
		t.Fatal("expected bundle digest to change after special knowledge update")
	}
	if first.ProfileDigests["developer"] == second.ProfileDigests["developer"] {
		t.Fatal("expected profile digest to change after special knowledge update")
	}
}

func TestResolvePathsRelativeToConfig(t *testing.T) {
	projectDir := t.TempDir()
	agentsDir := filepath.Join(projectDir, "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "developer.yaml"), []byte("name: developer\npurpose: Build software\nmodel: test-model\n"), 0644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}
	publicKeyPath := filepath.Join(projectDir, "bundle.pub")

	td := &FabricDef{
		Fabric:    FabricMeta{Name: "demo", Version: 1},
		AgentsDir: "agents",
		Storage: FabricStorage{
			SharedRoot:  "volumes/shared",
			AgentRoot:   "volumes/agents",
			ScratchRoot: "volumes/scratch",
		},
		Identity: FabricIdentity{
			Provider:    "mounted",
			TrustDomain: "spiffe.demo.internal",
			Mounted: FabricMountedIdentity{
				CertFile:   "identity/cert.pem",
				KeyFile:    "identity/key.pem",
				BundleFile: "identity/bundle.pem",
			},
		},
		Security: FabricSecurity{
			TrustedBundlePublicKeys: []string{"bundle.pub"},
		},
	}

	if err := ResolvePathsRelativeToConfig(td, filepath.Join(projectDir, "agents.yaml")); err != nil {
		t.Fatalf("ResolvePathsRelativeToConfig: %v", err)
	}

	if td.AgentsDir != agentsDir {
		t.Fatalf("AgentsDir = %q, want %q", td.AgentsDir, agentsDir)
	}
	if len(td.Security.TrustedBundlePublicKeys) != 1 || td.Security.TrustedBundlePublicKeys[0] != publicKeyPath {
		t.Fatalf("trusted bundle public keys = %v, want [%s]", td.Security.TrustedBundlePublicKeys, publicKeyPath)
	}
	if td.Storage.SharedRoot != filepath.Join(projectDir, "volumes", "shared") {
		t.Fatalf("shared root = %q", td.Storage.SharedRoot)
	}
	if td.Storage.AgentRoot != filepath.Join(projectDir, "volumes", "agents") {
		t.Fatalf("agent root = %q", td.Storage.AgentRoot)
	}
	if td.Storage.ScratchRoot != filepath.Join(projectDir, "volumes", "scratch") {
		t.Fatalf("scratch root = %q", td.Storage.ScratchRoot)
	}
	if td.Identity.Mounted.CertFile != filepath.Join(projectDir, "identity", "cert.pem") {
		t.Fatalf("identity cert file = %q", td.Identity.Mounted.CertFile)
	}
	if td.Identity.Mounted.KeyFile != filepath.Join(projectDir, "identity", "key.pem") {
		t.Fatalf("identity key file = %q", td.Identity.Mounted.KeyFile)
	}
	if td.Identity.Mounted.BundleFile != filepath.Join(projectDir, "identity", "bundle.pem") {
		t.Fatalf("identity bundle file = %q", td.Identity.Mounted.BundleFile)
	}
	if len(td.Agents) != 1 || td.Agents[0].Name != "developer" {
		t.Fatalf("resolved agents = %+v, want developer", td.Agents)
	}
}

func TestVerifySignedBundle(t *testing.T) {
	projectDir := t.TempDir()
	agentsDir := filepath.Join(projectDir, "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "developer.yaml"), []byte("name: developer\npurpose: Build software\nmodel: test-model\n"), 0644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	publicKey, privateKey, err := GenerateBundleKeyPair()
	if err != nil {
		t.Fatalf("GenerateBundleKeyPair: %v", err)
	}
	publicKeyPath := filepath.Join(projectDir, "bundle.pub")
	if err := WriteBundlePublicKey(publicKeyPath, publicKey); err != nil {
		t.Fatalf("WriteBundlePublicKey: %v", err)
	}

	td := &FabricDef{
		Fabric:    FabricMeta{Name: "demo", Version: 1},
		AgentsDir: agentsDir,
		Security: FabricSecurity{
			TrustedBundlePublicKeys: []string{publicKeyPath},
			RequireSignedBundles:    true,
		},
		Agents: []runtime.AgentDefinition{
			{Name: "developer", Purpose: "Build software", Model: "test-model"},
		},
	}

	manifest, err := GenerateManifest(agentsDir)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	if err := WriteManifest(ManifestPath(agentsDir), manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	bundle, err := GenerateSignedBundle(td)
	if err != nil {
		t.Fatalf("GenerateSignedBundle: %v", err)
	}
	if err := SignBundle(bundle, privateKey); err != nil {
		t.Fatalf("SignBundle: %v", err)
	}
	if err := WriteSignedBundle(BundlePath(agentsDir), bundle); err != nil {
		t.Fatalf("WriteSignedBundle: %v", err)
	}

	result, err := VerifySignedBundle(td)
	if err != nil {
		t.Fatalf("VerifySignedBundle: %v", err)
	}
	if !result.SignatureVerified {
		t.Fatal("expected signature verification to pass")
	}

	if err := os.WriteFile(filepath.Join(agentsDir, "developer.yaml"), []byte("name: developer\npurpose: Tampered\nmodel: test-model\n"), 0644); err != nil {
		t.Fatalf("tamper agent file: %v", err)
	}
	if _, err := VerifySignedBundle(td); err == nil {
		t.Fatal("expected VerifySignedBundle to fail after tampering")
	}
}

func TestVerifyBundleSignatureSkipsMalformedTrustedSignature(t *testing.T) {
	publicKey, privateKey, err := GenerateBundleKeyPair()
	if err != nil {
		t.Fatalf("GenerateBundleKeyPair: %v", err)
	}

	bundle := &SignedBundle{
		Version:        1,
		FabricName:     "demo",
		FabricVersion:  1,
		BundleDigest:   "bundle-digest",
		ProfileDigests: map[string]string{"developer": "profile-digest"},
		Checksums:      map[string]string{"developer.yaml": "abc123"},
	}
	if err := SignBundle(bundle, privateKey); err != nil {
		t.Fatalf("SignBundle: %v", err)
	}

	validSignature := bundle.Signatures[0]
	bundle.Signatures = []BundleSignature{
		{
			KeyID:     validSignature.KeyID,
			Algorithm: validSignature.Algorithm,
			Signature: hex.EncodeToString([]byte("not-a-valid-ed25519-signature")),
		},
		validSignature,
	}

	publicKeys := map[string]ed25519.PublicKey{
		bundleKeyID(publicKey): publicKey,
	}
	if err := VerifyBundleSignature(bundle, publicKeys); err != nil {
		t.Fatalf("VerifyBundleSignature: %v", err)
	}
}
