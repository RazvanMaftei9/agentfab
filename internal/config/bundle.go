package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type BundleFingerprint struct {
	BundleDigest   string
	ProfileDigests map[string]string
}

type bundleFingerprintPayload struct {
	Fabric    FabricMeta                           `json:"fabric"`
	Providers map[string]ProviderDef               `json:"providers,omitempty"`
	Defaults  FabricDefaults                       `json:"defaults,omitempty"`
	Profiles  map[string]profileFingerprintPayload `json:"profiles"`
}

type profileFingerprintPayload struct {
	Definition       runtime.AgentDefinition `json:"definition"`
	SpecialKnowledge string                  `json:"special_knowledge,omitempty"`
}

func ComputeBundleFingerprint(td *FabricDef) (BundleFingerprint, error) {
	if td == nil {
		return BundleFingerprint{}, fmt.Errorf("fabric definition is required")
	}

	payload := bundleFingerprintPayload{
		Fabric:    td.Fabric,
		Providers: td.Providers,
		Defaults:  td.Defaults,
		Profiles:  make(map[string]profileFingerprintPayload, len(td.Agents)),
	}
	profileDigests := make(map[string]string, len(td.Agents))

	for _, def := range td.Agents {
		if def.Name == "" {
			return BundleFingerprint{}, fmt.Errorf("agent definition name is required")
		}

		profilePayload, err := profilePayloadFor(td, def)
		if err != nil {
			return BundleFingerprint{}, err
		}
		profileDigest, err := stableDigest(profilePayload)
		if err != nil {
			return BundleFingerprint{}, fmt.Errorf("compute digest for profile %q: %w", def.Name, err)
		}

		payload.Profiles[def.Name] = profilePayload
		profileDigests[def.Name] = profileDigest
	}

	bundleDigest, err := stableDigest(payload)
	if err != nil {
		return BundleFingerprint{}, fmt.Errorf("compute bundle digest: %w", err)
	}

	return BundleFingerprint{
		BundleDigest:   bundleDigest,
		ProfileDigests: profileDigests,
	}, nil
}

func profilePayloadFor(td *FabricDef, def runtime.AgentDefinition) (profileFingerprintPayload, error) {
	payload := profileFingerprintPayload{Definition: def}
	if td == nil || def.SpecialKnowledgeFile == "" {
		return payload, nil
	}

	content, err := readSpecialKnowledge(td.AgentsDir, def.SpecialKnowledgeFile)
	if err != nil {
		return profileFingerprintPayload{}, fmt.Errorf("load special knowledge for profile %q: %w", def.Name, err)
	}
	payload.SpecialKnowledge = content
	return payload, nil
}

func readSpecialKnowledge(agentsDir, file string) (string, error) {
	if file == "" {
		return "", nil
	}

	path := file
	if agentsDir != "" && !filepath.IsAbs(path) {
		path = filepath.Join(agentsDir, file)
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func stableDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
