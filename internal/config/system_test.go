package config

import (
	"testing"
)

func TestFabricDefinitionRoundTrip(t *testing.T) {
	td := DefaultFabricDef("test-fabric")

	data, err := MarshalFabricDef(td)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseFabricDef(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.Fabric.Name != td.Fabric.Name {
		t.Errorf("name: got %q, want %q", parsed.Fabric.Name, td.Fabric.Name)
	}
	if parsed.Fabric.Version != td.Fabric.Version {
		t.Errorf("version: got %d, want %d", parsed.Fabric.Version, td.Fabric.Version)
	}
	if len(parsed.Agents) != len(td.Agents) {
		t.Errorf("agents: got %d, want %d", len(parsed.Agents), len(td.Agents))
	}
	for i, a := range parsed.Agents {
		if a.Name != td.Agents[i].Name {
			t.Errorf("agent %d name: got %q, want %q", i, a.Name, td.Agents[i].Name)
		}
		if a.Model != td.Agents[i].Model {
			t.Errorf("agent %d model: got %q, want %q", i, a.Model, td.Agents[i].Model)
		}
	}
}

func TestFabricDefinitionValidation(t *testing.T) {
	// Missing system name.
	_, err := ParseFabricDef([]byte("fabric:\n  version: 1\nagents: []"))
	if err == nil {
		t.Fatal("expected error for missing fabric name")
	}
}

func TestResolveAgentsFromEmbedded(t *testing.T) {
	td := &FabricDef{
		Fabric: FabricMeta{Name: "test", Version: 1},
	}
	if err := td.ResolveAgents(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(td.Agents) != 4 {
		t.Fatalf("expected 4 agents from embedded, got %d", len(td.Agents))
	}
}

func TestResolveAgentsAlwaysLoadsFromDir(t *testing.T) {
	td := &FabricDef{
		Fabric: FabricMeta{Name: "test", Version: 1},
		// No AgentsDir set — should fall back to embedded defaults.
	}
	if err := td.ResolveAgents(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(td.Agents) != 4 {
		t.Errorf("expected 4 agents from embedded fallback, got %d", len(td.Agents))
	}
}

func TestParseMinimalFabricYAML(t *testing.T) {
	// An agents.yaml with only system name and agents_dir should resolve from embedded defaults.
	data := []byte("fabric:\n  name: minimal\n  version: 1\n")
	td, err := ParseFabricDef(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(td.Agents) != 4 {
		t.Errorf("expected 4 agents from embedded fallback, got %d", len(td.Agents))
	}
}

func TestFabricDefinitionFileRoundTrip(t *testing.T) {
	td := DefaultFabricDef("file-test")
	path := t.TempDir() + "/agents.yaml"

	if err := WriteFabricDef(path, td); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := LoadFabricDef(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Fabric.Name != td.Fabric.Name {
		t.Errorf("name mismatch: got %q, want %q", loaded.Fabric.Name, td.Fabric.Name)
	}
	if len(loaded.Agents) != len(td.Agents) {
		t.Errorf("agents count: got %d, want %d", len(loaded.Agents), len(td.Agents))
	}
}

func TestFabricDefinitionControlPlaneRoundTrip(t *testing.T) {
	td := DefaultFabricDef("cp-test")
	td.ControlPlane = FabricControlPlane{
		Backend: "etcd",
		API: FabricControlPlaneAPI{
			Address:       "cp.agentfab.internal:50051",
			ListenAddress: ":50051",
		},
		Etcd: FabricControlPlaneEtcd{
			Endpoints:   []string{"http://127.0.0.1:2379", "http://127.0.0.1:2381"},
			DialTimeout: "7s",
		},
	}
	td.Storage = FabricStorage{
		SharedRoot:  "/mnt/shared",
		AgentRoot:   "/mnt/agents",
		ScratchRoot: "/mnt/scratch",
	}
	td.Identity = FabricIdentity{
		Provider:    "mounted",
		TrustDomain: "spiffe.example.internal",
		Mounted: FabricMountedIdentity{
			CertFile:   "/var/run/agentfab/cert.pem",
			KeyFile:    "/var/run/agentfab/key.pem",
			BundleFile: "/var/run/agentfab/bundle.pem",
		},
	}

	data, err := MarshalFabricDef(td)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseFabricDef(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.ControlPlane.Backend != "etcd" {
		t.Fatalf("backend = %q, want etcd", parsed.ControlPlane.Backend)
	}
	if parsed.ControlPlane.API.Address != "cp.agentfab.internal:50051" {
		t.Fatalf("api address = %q, want cp.agentfab.internal:50051", parsed.ControlPlane.API.Address)
	}
	if parsed.ControlPlane.API.ListenAddress != ":50051" {
		t.Fatalf("listen address = %q, want :50051", parsed.ControlPlane.API.ListenAddress)
	}
	if len(parsed.ControlPlane.Etcd.Endpoints) != 2 {
		t.Fatalf("endpoints = %v, want 2 endpoints", parsed.ControlPlane.Etcd.Endpoints)
	}
	if parsed.ControlPlane.Etcd.DialTimeout != "7s" {
		t.Fatalf("dial timeout = %q, want 7s", parsed.ControlPlane.Etcd.DialTimeout)
	}
	if parsed.Storage.SharedRoot != "/mnt/shared" {
		t.Fatalf("shared root = %q, want /mnt/shared", parsed.Storage.SharedRoot)
	}
	if parsed.Storage.AgentRoot != "/mnt/agents" {
		t.Fatalf("agent root = %q, want /mnt/agents", parsed.Storage.AgentRoot)
	}
	if parsed.Storage.ScratchRoot != "/mnt/scratch" {
		t.Fatalf("scratch root = %q, want /mnt/scratch", parsed.Storage.ScratchRoot)
	}
	if parsed.Identity.Provider != "mounted" {
		t.Fatalf("identity provider = %q, want mounted", parsed.Identity.Provider)
	}
	if parsed.Identity.TrustDomain != "spiffe.example.internal" {
		t.Fatalf("identity trust domain = %q, want spiffe.example.internal", parsed.Identity.TrustDomain)
	}
	if parsed.Identity.Mounted.CertFile != "/var/run/agentfab/cert.pem" {
		t.Fatalf("identity cert file = %q, want /var/run/agentfab/cert.pem", parsed.Identity.Mounted.CertFile)
	}
}

func TestProvidersRoundTrip(t *testing.T) {
	td := DefaultFabricDef("provider-test")
	td.Providers = map[string]ProviderDef{
		"anthropic": {APIKeyEnv: "MY_CLAUDE_KEY"},
		"ollama":    {Type: "openai-compat", BaseURLEnv: "OLLAMA_URL"},
		"together":  {Type: "openai-compat", APIKeyEnv: "TOGETHER_KEY", BaseURLEnv: "TOGETHER_URL"},
	}

	data, err := MarshalFabricDef(td)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseFabricDef(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(parsed.Providers) != 3 {
		t.Fatalf("providers count: got %d, want 3", len(parsed.Providers))
	}

	// Check anthropic override.
	if p, ok := parsed.Providers["anthropic"]; !ok {
		t.Error("missing anthropic provider")
	} else {
		if p.APIKeyEnv != "MY_CLAUDE_KEY" {
			t.Errorf("anthropic api_key_env: got %q, want %q", p.APIKeyEnv, "MY_CLAUDE_KEY")
		}
		if p.Type != "" {
			t.Errorf("anthropic type: got %q, want empty", p.Type)
		}
	}

	// Check ollama alias.
	if p, ok := parsed.Providers["ollama"]; !ok {
		t.Error("missing ollama provider")
	} else {
		if p.Type != "openai-compat" {
			t.Errorf("ollama type: got %q, want %q", p.Type, "openai-compat")
		}
		if p.BaseURLEnv != "OLLAMA_URL" {
			t.Errorf("ollama base_url_env: got %q, want %q", p.BaseURLEnv, "OLLAMA_URL")
		}
	}

	// Check together alias.
	if p, ok := parsed.Providers["together"]; !ok {
		t.Error("missing together provider")
	} else {
		if p.Type != "openai-compat" {
			t.Errorf("together type: got %q, want %q", p.Type, "openai-compat")
		}
		if p.APIKeyEnv != "TOGETHER_KEY" {
			t.Errorf("together api_key_env: got %q, want %q", p.APIKeyEnv, "TOGETHER_KEY")
		}
		if p.BaseURLEnv != "TOGETHER_URL" {
			t.Errorf("together base_url_env: got %q, want %q", p.BaseURLEnv, "TOGETHER_URL")
		}
	}
}
