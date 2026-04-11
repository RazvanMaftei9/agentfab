package config

import (
	"fmt"
	"os"

	"github.com/razvanmaftei/agentfab/defaults"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"gopkg.in/yaml.v3"
)

// ProviderDef defines a named provider alias with optional env var overrides.
// Custom aliases must set Type to a built-in provider name.
type ProviderDef struct {
	Type       string `yaml:"type,omitempty" json:"type,omitempty"`
	APIKeyEnv  string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	BaseURLEnv string `yaml:"base_url_env,omitempty" json:"base_url_env,omitempty"`
}

// FabricDef is the top-level agents.yaml schema.
type FabricDef struct {
	Fabric       FabricMeta                `yaml:"fabric" json:"fabric"`
	AgentsDir    string                    `yaml:"agents_dir,omitempty" json:"agents_dir,omitempty"`
	Providers    map[string]ProviderDef    `yaml:"providers,omitempty" json:"providers,omitempty"`
	Agents       []runtime.AgentDefinition `yaml:"-" json:"-"`
	Defaults     FabricDefaults            `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Storage      FabricStorage             `yaml:"storage,omitempty" json:"storage,omitempty"`
	ControlPlane FabricControlPlane        `yaml:"control_plane,omitempty" json:"control_plane,omitempty"`
	Identity     FabricIdentity            `yaml:"identity,omitempty" json:"identity,omitempty"`
	Security     FabricSecurity            `yaml:"security,omitempty" json:"security,omitempty"`
}

type FabricMeta struct {
	Name    string `yaml:"name" json:"name"`
	Version int    `yaml:"version" json:"version"`
}

type FabricDefaults struct {
	EscalationTarget string          `yaml:"escalation_target,omitempty" json:"escalation_target,omitempty"`
	Budget           *runtime.Budget `yaml:"budget,omitempty" json:"budget,omitempty"`
}

type FabricSecurity struct {
	TrustedBundlePublicKeys []string `yaml:"trusted_bundle_public_keys,omitempty" json:"trusted_bundle_public_keys,omitempty"`
	RequireSignedBundles    bool     `yaml:"require_signed_bundles,omitempty" json:"require_signed_bundles,omitempty"`
}

type FabricStorage struct {
	SharedRoot  string `yaml:"shared_root,omitempty" json:"shared_root,omitempty"`
	AgentRoot   string `yaml:"agent_root,omitempty" json:"agent_root,omitempty"`
	ScratchRoot string `yaml:"scratch_root,omitempty" json:"scratch_root,omitempty"`
}

type FabricControlPlane struct {
	Backend string                 `yaml:"backend,omitempty" json:"backend,omitempty"`
	API     FabricControlPlaneAPI  `yaml:"api,omitempty" json:"api,omitempty"`
	Etcd    FabricControlPlaneEtcd `yaml:"etcd,omitempty" json:"etcd,omitempty"`
}

type FabricControlPlaneAPI struct {
	Address       string `yaml:"address,omitempty" json:"address,omitempty"`
	ListenAddress string `yaml:"listen_address,omitempty" json:"listen_address,omitempty"`
}

type FabricControlPlaneEtcd struct {
	Endpoints   []string `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
	DialTimeout string   `yaml:"dial_timeout,omitempty" json:"dial_timeout,omitempty"`
}

type FabricIdentity struct {
	Provider    string                `yaml:"provider,omitempty" json:"provider,omitempty"`
	TrustDomain string                `yaml:"trust_domain,omitempty" json:"trust_domain,omitempty"`
	Mounted     FabricMountedIdentity `yaml:"mounted,omitempty" json:"mounted,omitempty"`
}

type FabricMountedIdentity struct {
	CertFile   string `yaml:"cert_file,omitempty" json:"cert_file,omitempty"`
	KeyFile    string `yaml:"key_file,omitempty" json:"key_file,omitempty"`
	BundleFile string `yaml:"bundle_file,omitempty" json:"bundle_file,omitempty"`
}

// LoadFabricDef reads and parses an agents.yaml file.
func LoadFabricDef(path string) (*FabricDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fabric definition: %w", err)
	}
	return ParseFabricDef(data)
}

func ParseFabricDef(data []byte) (*FabricDef, error) {
	var td FabricDef
	if err := yaml.Unmarshal(data, &td); err != nil {
		return nil, fmt.Errorf("parse fabric definition: %w", err)
	}
	if td.Fabric.Name == "" {
		return nil, fmt.Errorf("fabric name is required")
	}
	if err := td.ResolveAgents(); err != nil {
		return nil, fmt.Errorf("resolve agents: %w", err)
	}
	if err := ValidateAgentSet(td.Agents); err != nil {
		return nil, fmt.Errorf("validate fabric agents: %w", err)
	}
	if err := ValidateAgentProviders(td.Agents, td.Providers); err != nil {
		return nil, fmt.Errorf("validate agent providers: %w", err)
	}
	return &td, nil
}

// ResolveAgents populates Agents from AgentsDir or embedded defaults.
func (td *FabricDef) ResolveAgents() error {
	if td.AgentsDir != "" {
		if info, err := os.Stat(td.AgentsDir); err == nil && info.IsDir() {
			agents, err := LoadAgentsDir(td.AgentsDir)
			if err != nil {
				return fmt.Errorf("load agents from %q: %w", td.AgentsDir, err)
			}
			if len(agents) > 0 {
				td.Agents = agents
				return nil
			}
		}
	}

	agents, err := LoadAgentsDirFS(defaults.AgentFS, "agents")
	if err != nil {
		return fmt.Errorf("load embedded default agents: %w", err)
	}
	td.Agents = agents
	return nil
}

func WriteFabricDef(path string, td *FabricDef) error {
	data, err := MarshalFabricDef(td)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func MarshalFabricDef(td *FabricDef) ([]byte, error) {
	// Write only the fabric-level config; agent definitions live in agents_dir.
	out := &FabricDef{
		Fabric:       td.Fabric,
		AgentsDir:    td.AgentsDir,
		Providers:    td.Providers,
		Defaults:     td.Defaults,
		Storage:      td.Storage,
		ControlPlane: td.ControlPlane,
		Identity:     td.Identity,
		Security:     td.Security,
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal fabric definition: %w", err)
	}
	return data, nil
}
