package identity

import (
	"testing"

	"github.com/razvanmaftei/agentfab/internal/config"
)

func TestProviderFromFabricDefaultsToLocalDev(t *testing.T) {
	systemDef := &config.FabricDef{
		Fabric: config.FabricMeta{Name: "demo", Version: 1},
	}

	provider, err := ProviderFromFabric(systemDef, t.TempDir())
	if err != nil {
		t.Fatalf("ProviderFromFabric: %v", err)
	}
	if provider.Name() != "local-dev" {
		t.Fatalf("provider = %q, want local-dev", provider.Name())
	}
}

func TestProviderFromFabricMounted(t *testing.T) {
	systemDef := &config.FabricDef{
		Fabric: config.FabricMeta{Name: "demo", Version: 1},
		Identity: config.FabricIdentity{
			Provider:    "mounted",
			TrustDomain: "spiffe.demo.internal",
			Mounted: config.FabricMountedIdentity{
				CertFile:   "/tmp/cert.pem",
				KeyFile:    "/tmp/key.pem",
				BundleFile: "/tmp/bundle.pem",
			},
		},
	}

	provider, err := ProviderFromFabric(systemDef, t.TempDir())
	if err != nil {
		t.Fatalf("ProviderFromFabric: %v", err)
	}
	if provider.Name() != "mounted" {
		t.Fatalf("provider = %q, want mounted", provider.Name())
	}
	if SupportsArbitrarySubjects(provider) {
		t.Fatal("mounted provider should not advertise arbitrary subject issuance")
	}
}
