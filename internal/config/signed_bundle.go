package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	bundleFormatVersion = 1
	bundleAlgorithm     = "ed25519"
)

type SignedBundle struct {
	Version        int               `json:"version"`
	GeneratedAt    string            `json:"generated_at"`
	FabricName     string            `json:"fabric_name"`
	FabricVersion  int               `json:"fabric_version"`
	Checksums      map[string]string `json:"checksums"`
	BundleDigest   string            `json:"bundle_digest"`
	ProfileDigests map[string]string `json:"profile_digests"`
	Signatures     []BundleSignature `json:"signatures,omitempty"`
}

type BundleSignature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

type VerifiedBundle struct {
	BundleDigest   string
	ProfileDigests map[string]string
}

type BundleVerificationResult struct {
	VerifiedBundle
	ManifestVerified  bool
	SignedBundleUsed  bool
	SignatureVerified bool
}

func BundlePath(agentsDir string) string {
	return filepath.Join(agentsDir, "bundle.json")
}

func GenerateSignedBundle(td *FabricDef) (*SignedBundle, error) {
	if td == nil {
		return nil, fmt.Errorf("fabric definition is required")
	}
	if td.AgentsDir == "" {
		return nil, fmt.Errorf("agents directory is required")
	}

	manifest, err := GenerateManifest(td.AgentsDir)
	if err != nil {
		return nil, err
	}
	fingerprint, err := ComputeBundleFingerprint(td)
	if err != nil {
		return nil, err
	}

	return &SignedBundle{
		Version:        bundleFormatVersion,
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		FabricName:     td.Fabric.Name,
		FabricVersion:  td.Fabric.Version,
		Checksums:      manifest.Checksums,
		BundleDigest:   fingerprint.BundleDigest,
		ProfileDigests: fingerprint.ProfileDigests,
	}, nil
}

func WriteSignedBundle(path string, bundle *SignedBundle) error {
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal signed bundle: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func LoadSignedBundle(path string) (*SignedBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signed bundle: %w", err)
	}
	var bundle SignedBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("parse signed bundle: %w", err)
	}
	return &bundle, nil
}

func SignBundle(bundle *SignedBundle, privateKey ed25519.PrivateKey) error {
	if bundle == nil {
		return fmt.Errorf("bundle is required")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid Ed25519 private key")
	}

	payload, err := bundlePayload(bundle)
	if err != nil {
		return err
	}

	publicKey := privateKey.Public().(ed25519.PublicKey)
	signature := ed25519.Sign(privateKey, payload)
	keyID := bundleKeyID(publicKey)

	filtered := bundle.Signatures[:0]
	for _, existing := range bundle.Signatures {
		if existing.KeyID == keyID {
			continue
		}
		filtered = append(filtered, existing)
	}
	bundle.Signatures = append(filtered, BundleSignature{
		KeyID:     keyID,
		Algorithm: bundleAlgorithm,
		Signature: hex.EncodeToString(signature),
	})
	sort.Slice(bundle.Signatures, func(i, j int) bool {
		return bundle.Signatures[i].KeyID < bundle.Signatures[j].KeyID
	})
	return nil
}

func VerifySignedBundle(td *FabricDef) (BundleVerificationResult, error) {
	if td == nil {
		return BundleVerificationResult{}, fmt.Errorf("fabric definition is required")
	}
	fingerprint, err := ComputeBundleFingerprint(td)
	if err != nil {
		return BundleVerificationResult{}, err
	}

	result := BundleVerificationResult{
		VerifiedBundle: VerifiedBundle{
			BundleDigest:   fingerprint.BundleDigest,
			ProfileDigests: fingerprint.ProfileDigests,
		},
	}

	if td.AgentsDir == "" {
		return result, nil
	}

	if len(td.Security.TrustedBundlePublicKeys) == 0 && !td.Security.RequireSignedBundles {
		ok, _, err := VerifyAgentsManifest(td.AgentsDir)
		if err == nil && ok {
			result.ManifestVerified = true
		}
		return result, nil
	}

	if len(td.Security.TrustedBundlePublicKeys) == 0 {
		return BundleVerificationResult{}, fmt.Errorf("signed bundles are required but no trusted bundle public keys are configured")
	}

	bundle, err := LoadSignedBundle(BundlePath(td.AgentsDir))
	if err != nil {
		return BundleVerificationResult{}, err
	}
	if err := validateBundleAgainstFabric(td, bundle, fingerprint); err != nil {
		return BundleVerificationResult{}, err
	}

	publicKeys, err := LoadBundlePublicKeys(td.Security.TrustedBundlePublicKeys)
	if err != nil {
		return BundleVerificationResult{}, err
	}
	if err := VerifyBundleSignature(bundle, publicKeys); err != nil {
		return BundleVerificationResult{}, err
	}

	result.SignedBundleUsed = true
	result.SignatureVerified = true
	result.VerifiedBundle.BundleDigest = bundle.BundleDigest
	result.VerifiedBundle.ProfileDigests = cloneChecksums(bundle.ProfileDigests)
	if ok, _, err := VerifyAgentsManifest(td.AgentsDir); err == nil && ok {
		result.ManifestVerified = true
	}
	return result, nil
}

func GenerateBundleKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func WriteBundlePrivateKey(path string, privateKey ed25519.PrivateKey) error {
	data, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: data}
	return os.WriteFile(path, pem.EncodeToMemory(block), 0600)
}

func WriteBundlePublicKey(path string, publicKey ed25519.PublicKey) error {
	data, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: data}
	return os.WriteFile(path, pem.EncodeToMemory(block), 0644)
}

func LoadBundlePrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse private key PEM: no block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not Ed25519")
	}
	return privateKey, nil
}

func LoadBundlePublicKeys(paths []string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		publicKey, err := LoadBundlePublicKey(path)
		if err != nil {
			return nil, err
		}
		keys[bundleKeyID(publicKey)] = publicKey
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no trusted bundle public keys loaded")
	}
	return keys, nil
}

func LoadBundlePublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse public key PEM: no block found")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	publicKey, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not Ed25519")
	}
	return publicKey, nil
}

func VerifyBundleSignature(bundle *SignedBundle, publicKeys map[string]ed25519.PublicKey) error {
	if bundle == nil {
		return fmt.Errorf("bundle is required")
	}
	if len(bundle.Signatures) == 0 {
		return fmt.Errorf("signed bundle has no signatures")
	}
	payload, err := bundlePayload(bundle)
	if err != nil {
		return err
	}
	for _, signature := range bundle.Signatures {
		if signature.Algorithm != bundleAlgorithm {
			continue
		}
		publicKey, ok := publicKeys[signature.KeyID]
		if !ok {
			continue
		}
		rawSignature, err := hex.DecodeString(signature.Signature)
		if err != nil {
			continue
		}
		if ed25519.Verify(publicKey, payload, rawSignature) {
			return nil
		}
	}
	return fmt.Errorf("signed bundle does not verify against trusted public keys")
}

func validateBundleAgainstFabric(td *FabricDef, bundle *SignedBundle, fingerprint BundleFingerprint) error {
	if bundle.FabricName != td.Fabric.Name {
		return fmt.Errorf("signed bundle fabric %q does not match fabric %q", bundle.FabricName, td.Fabric.Name)
	}
	if bundle.FabricVersion != td.Fabric.Version {
		return fmt.Errorf("signed bundle fabric version %d does not match fabric version %d", bundle.FabricVersion, td.Fabric.Version)
	}
	ok, mismatches, err := VerifyManifest(td.AgentsDir, &Manifest{
		Version:     bundle.Version,
		GeneratedAt: bundle.GeneratedAt,
		Checksums:   bundle.Checksums,
	})
	if err != nil {
		return fmt.Errorf("verify signed bundle manifest: %w", err)
	}
	if !ok {
		return fmt.Errorf("signed bundle manifest does not match local files: %v", mismatches)
	}
	if bundle.BundleDigest != fingerprint.BundleDigest {
		return fmt.Errorf("signed bundle digest %q does not match local bundle digest %q", bundle.BundleDigest, fingerprint.BundleDigest)
	}
	if !equalChecksums(bundle.ProfileDigests, fingerprint.ProfileDigests) {
		return fmt.Errorf("signed bundle profile digests do not match local profile digests")
	}
	return nil
}

func bundlePayload(bundle *SignedBundle) ([]byte, error) {
	if bundle == nil {
		return nil, fmt.Errorf("bundle is required")
	}
	payload := struct {
		Version        int               `json:"version"`
		GeneratedAt    string            `json:"generated_at"`
		FabricName     string            `json:"fabric_name"`
		FabricVersion  int               `json:"fabric_version"`
		Checksums      map[string]string `json:"checksums"`
		BundleDigest   string            `json:"bundle_digest"`
		ProfileDigests map[string]string `json:"profile_digests"`
	}{
		Version:        bundle.Version,
		GeneratedAt:    bundle.GeneratedAt,
		FabricName:     bundle.FabricName,
		FabricVersion:  bundle.FabricVersion,
		Checksums:      bundle.Checksums,
		BundleDigest:   bundle.BundleDigest,
		ProfileDigests: bundle.ProfileDigests,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal bundle payload: %w", err)
	}
	return data, nil
}

func bundleKeyID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:8])
}

func cloneChecksums(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func equalChecksums(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if right[key] != leftValue {
			return false
		}
	}
	return true
}
