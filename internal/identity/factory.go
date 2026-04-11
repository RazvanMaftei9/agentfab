package identity

import (
	"fmt"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/config"
)

const defaultTrustDomain = "agentfab.local"

func ProviderFromFabric(systemDef *config.FabricDef, dataDir string) (CertificateProvider, error) {
	if systemDef == nil {
		return nil, fmt.Errorf("fabric definition is required")
	}

	providerType := strings.TrimSpace(systemDef.Identity.Provider)
	if providerType == "" {
		providerType = "local-dev"
	}

	trustDomain := TrustDomainFromFabric(systemDef)

	switch providerType {
	case "local-dev":
		return NewSharedLocalDevProvider(dataDir, trustDomain)
	case "mounted":
		return NewMountedProvider(
			trustDomain,
			strings.TrimSpace(systemDef.Identity.Mounted.CertFile),
			strings.TrimSpace(systemDef.Identity.Mounted.KeyFile),
			strings.TrimSpace(systemDef.Identity.Mounted.BundleFile),
		)
	default:
		return nil, fmt.Errorf("unsupported identity provider %q", providerType)
	}
}

func TrustDomainFromFabric(systemDef *config.FabricDef) string {
	if systemDef == nil {
		return defaultTrustDomain
	}
	trustDomain := strings.TrimSpace(systemDef.Identity.TrustDomain)
	if trustDomain == "" {
		return defaultTrustDomain
	}
	return trustDomain
}

func SupportsArbitrarySubjects(provider CertificateProvider) bool {
	capable, ok := provider.(interface{ SupportsArbitrarySubjects() bool })
	if !ok {
		return true
	}
	return capable.SupportsArbitrarySubjects()
}
