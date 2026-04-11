package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/controlplanesvc"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/nodehost"
)

const localClusterStartupTimeout = 15 * time.Second

type localCluster struct {
	controlPlaneAddress string
	store               controlplane.Store
	server              *agentgrpc.Server
	serverIdentity      *identity.ManagedCertificate
	nodeHosts           []*nodehost.Host
	debugStores         []*llm.DebugStore
}

func resolveBootstrapNodeCount(systemDef *config.FabricDef, externalNodes bool, explicitControlPlaneAddress string, requestedCount int, requestedCountSet bool) (int, error) {
	if requestedCount < 0 {
		return 0, fmt.Errorf("--bootstrap-nodes must be zero or greater")
	}
	if !externalNodes {
		if requestedCountSet && requestedCount > 0 {
			return 0, fmt.Errorf("--bootstrap-nodes requires --external-nodes")
		}
		return 0, nil
	}

	configuredControlPlaneAddress := ""
	if systemDef != nil {
		configuredControlPlaneAddress = systemDef.ControlPlane.API.Address
	}
	if explicitControlPlaneAddress != "" || configuredControlPlaneAddress != "" {
		if requestedCountSet && requestedCount > 0 {
			return 0, fmt.Errorf("--bootstrap-nodes cannot be used when a control-plane API address is already configured")
		}
		return 0, nil
	}

	if requestedCountSet {
		return requestedCount, nil
	}
	return 1, nil
}

func startLocalCluster(
	ctx context.Context,
	systemDef *config.FabricDef,
	dataDir string,
	verification config.BundleVerificationResult,
	nodeCount int,
	enableDebug bool,
	modelFactory nodehost.ModelFactory,
) (*localCluster, error) {
	if nodeCount < 1 {
		return nil, fmt.Errorf("bootstrap node count must be at least 1")
	}

	fingerprint, err := resolveBundleFingerprint(systemDef, verification)
	if err != nil {
		return nil, err
	}

	storeOptions, err := controlplane.BackendOptionsFromFabric(systemDef, dataDir)
	if err != nil {
		return nil, fmt.Errorf("build control-plane options: %w", err)
	}
	store, err := controlplane.NewStore(storeOptions)
	if err != nil {
		return nil, fmt.Errorf("create control-plane store: %w", err)
	}

	provider, err := identity.NewSharedLocalDevProvider(dataDir, identity.TrustDomainFromFabric(systemDef))
	if err != nil {
		closeControlPlaneStore(store)
		return nil, fmt.Errorf("create local identity provider: %w", err)
	}
	attestor := identity.NewLocalDevJoinTokenAuthority(dataDir, identity.TrustDomainFromFabric(systemDef))

	listenAddr := systemDef.ControlPlane.API.ListenAddress
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}
	serverIdentity, err := identity.NewManagedCertificate(ctx, provider, controlPlaneIdentityRequest(identity.TrustDomainFromFabric(systemDef), systemDef.Fabric.Name, listenAddr, listenAddr))
	if err != nil {
		closeControlPlaneStore(store)
		return nil, fmt.Errorf("issue control-plane identity: %w", err)
	}
	server, err := agentgrpc.NewServer("control-plane", listenAddr, 64, serverIdentity.ServerTLS())
	if err != nil {
		serverIdentity.Close()
		closeControlPlaneStore(store)
		return nil, fmt.Errorf("create control-plane gRPC server: %w", err)
	}
	server.SetControlPlaneService(controlplanesvc.New(controlplanesvc.Config{
		Store:                  store,
		Fabric:                 systemDef.Fabric.Name,
		ExpectedBundleDigest:   fingerprint.BundleDigest,
		ExpectedProfileDigests: fingerprint.ProfileDigests,
		Attestor:               attestor,
	}))
	go func() {
		_ = server.Serve()
	}()

	cluster := &localCluster{
		controlPlaneAddress: server.Addr(),
		store:               store,
		server:              server,
		serverIdentity:      serverIdentity,
	}

	for index := 0; index < nodeCount; index++ {
		nodeID := fmt.Sprintf("node-%d", index+1)
		host, debugStore, err := newBootstrapNodeHost(ctx, systemDef, dataDir, cluster.controlPlaneAddress, nodeID, fingerprint, enableDebug, attestor, modelFactory)
		if err != nil {
			_ = cluster.Shutdown(context.Background())
			return nil, err
		}
		cluster.nodeHosts = append(cluster.nodeHosts, host)
		if debugStore != nil {
			cluster.debugStores = append(cluster.debugStores, debugStore)
		}
	}

	if err := cluster.waitUntilReady(ctx, nodeCount, expectedBootstrapInstanceCount(systemDef, nodeCount)); err != nil {
		_ = cluster.Shutdown(context.Background())
		return nil, err
	}

	return cluster, nil
}

func newBootstrapNodeHost(
	ctx context.Context,
	systemDef *config.FabricDef,
	dataDir string,
	controlPlaneAddress string,
	nodeID string,
	fingerprint config.BundleFingerprint,
	enableDebug bool,
	attestor *identity.LocalDevJoinTokenAuthority,
	modelFactory nodehost.ModelFactory,
) (*nodehost.Host, *llm.DebugStore, error) {
	host, err := nodehost.New(systemDef, dataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("create node host %q: %w", nodeID, err)
	}

	measurements, err := bootstrapNodeMeasurements(fingerprint.BundleDigest)
	if err != nil {
		return nil, nil, err
	}
	token, err := attestor.IssueNodeToken(ctx, identity.NodeTokenRequest{
		Fabric:               systemDef.Fabric.Name,
		NodeID:               nodeID,
		Description:          "auto-generated local cluster enrollment token",
		ExpiresAt:            time.Now().Add(24 * time.Hour),
		Reusable:             true,
		ExpectedMeasurements: measurements,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("issue bootstrap token for %q: %w", nodeID, err)
	}

	host.NodeID = nodeID
	host.ListenHost = "127.0.0.1"
	host.AdvertiseHost = "127.0.0.1"
	host.ControlPlaneAddress = controlPlaneAddress
	host.NodeLabels = map[string]string{
		"role":       "worker",
		"mode":       "local-bootstrap",
		"node.index": bootstrapNodeIndex(nodeID),
	}
	host.ModelFactory = modelFactory
	host.BundleDigest = fingerprint.BundleDigest
	host.ProfileDigests = cloneStringMap(fingerprint.ProfileDigests)
	host.Attestation = identity.NodeAttestation{
		Type:  identity.NodeJoinTokenAttestationType,
		Token: token.Value,
	}

	var debugStore *llm.DebugStore
	if enableDebug {
		debugDir := dataDir + "/debug/" + nodeID
		debugStore, err = llm.NewDebugStore(debugDir)
		if err != nil {
			return nil, nil, fmt.Errorf("create debug store for %q: %w", nodeID, err)
		}
		host.DebugLog = debugStore
	}

	if err := host.Start(ctx); err != nil {
		if debugStore != nil {
			debugStore.Close()
		}
		return nil, nil, fmt.Errorf("start node host %q: %w", nodeID, err)
	}

	return host, debugStore, nil
}

func (c *localCluster) Shutdown(ctx context.Context) error {
	if c == nil {
		return nil
	}

	var firstErr error
	for _, host := range c.nodeHosts {
		if err := host.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.server != nil {
		c.server.Stop()
	}
	if c.serverIdentity != nil {
		c.serverIdentity.Close()
	}
	for _, store := range c.debugStores {
		if err := store.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	closeControlPlaneStore(c.store)
	return firstErr
}

func (c *localCluster) waitUntilReady(ctx context.Context, expectedNodes, expectedInstances int) error {
	waitCtx, cancel := context.WithTimeout(ctx, localClusterStartupTimeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		ready, err := c.isReady(waitCtx, expectedNodes, expectedInstances)
		if err == nil && ready {
			return nil
		}

		select {
		case <-waitCtx.Done():
			if err != nil {
				return fmt.Errorf("wait for local cluster readiness: %w", err)
			}
			return fmt.Errorf("wait for local cluster readiness: timed out")
		case <-ticker.C:
		}
	}
}

func (c *localCluster) isReady(ctx context.Context, expectedNodes, expectedInstances int) (bool, error) {
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return false, err
	}
	readyNodes := 0
	for _, node := range nodes {
		if node.State == controlplane.NodeStateReady {
			readyNodes++
		}
	}
	if readyNodes < expectedNodes {
		return false, nil
	}

	instances, err := c.store.ListInstances(ctx, controlplane.InstanceFilter{})
	if err != nil {
		return false, err
	}
	readyInstances := 0
	for _, instance := range instances {
		if instance.State == controlplane.InstanceStateReady || instance.State == controlplane.InstanceStateBusy {
			readyInstances++
		}
	}
	return readyInstances >= expectedInstances, nil
}

func resolveBundleFingerprint(systemDef *config.FabricDef, verification config.BundleVerificationResult) (config.BundleFingerprint, error) {
	if verification.BundleDigest != "" {
		return config.BundleFingerprint{
			BundleDigest:   verification.BundleDigest,
			ProfileDigests: cloneStringMap(verification.ProfileDigests),
		}, nil
	}
	return config.ComputeBundleFingerprint(systemDef)
}

func expectedBootstrapInstanceCount(systemDef *config.FabricDef, nodeCount int) int {
	if systemDef == nil || nodeCount < 1 {
		return 0
	}
	agentsPerNode := 0
	for _, def := range systemDef.Agents {
		if def.Name == "conductor" {
			continue
		}
		agentsPerNode++
	}
	return agentsPerNode * nodeCount
}

func bootstrapNodeMeasurements(bundleDigest string) (map[string]string, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryDigest, err := identity.FileSHA256(executablePath)
	if err != nil {
		return nil, fmt.Errorf("compute executable digest: %w", err)
	}
	measurements := map[string]string{
		"binary_sha256": binaryDigest,
	}
	if bundleDigest != "" {
		measurements["bundle_digest"] = bundleDigest
	}
	return measurements, nil
}

func bootstrapNodeIndex(nodeID string) string {
	parts := []rune(nodeID)
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] < '0' || parts[i] > '9' {
			if i == len(parts)-1 {
				return ""
			}
			return string(parts[i+1:])
		}
	}
	return nodeID
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func localBootstrapListenAddr(externalNodes bool, requestedListenAddr string, listenFlagSet bool) string {
	if !externalNodes || listenFlagSet {
		return requestedListenAddr
	}
	return "127.0.0.1:0"
}

func bootstrapNodeSummary(cluster *localCluster) []string {
	if cluster == nil {
		return nil
	}
	summary := make([]string, 0, len(cluster.nodeHosts))
	for _, host := range cluster.nodeHosts {
		summary = append(summary, host.NodeID)
	}
	return summary
}
