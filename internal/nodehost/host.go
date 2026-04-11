package nodehost

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/agent"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const heartbeatInterval = 5 * time.Second

type ModelFactory func(ctx context.Context, modelID string) (model.ChatModel, error)

type Host struct {
	SystemDef           *config.FabricDef
	DataDir             string
	NodeID              string
	ListenHost          string
	AdvertiseHost       string
	ControlPlaneAddress string
	NodeLabels          map[string]string
	MaxInstances        int
	MaxTasks            int
	Agents              []string
	StorageLayout       runtime.StorageLayout
	ControlPlane        controlplane.Store
	Discovery           runtime.Discovery
	Membership          controlplane.MembershipWriter
	ModelFactory        ModelFactory
	DebugLog            *llm.DebugStore
	Identity            identity.CertificateProvider
	Attestor            identity.Attestor
	Attestation         identity.NodeAttestation
	BundleDigest        string
	ProfileDigests      map[string]string

	mu           sync.Mutex
	started      bool
	startedAt    time.Time
	cancel       context.CancelFunc
	instances    map[string]*hostedInstance
	heartbeatWg  sync.WaitGroup
	nodeIdentity *identity.ManagedCertificate
}

type hostedInstance struct {
	instance   controlplane.AgentInstance
	server     *agentgrpc.Server
	comm       *agentgrpc.Communicator
	membership controlplane.MembershipWriter
	identity   *identity.ManagedCertificate
	cancel     context.CancelFunc
	done       chan error
}

func New(systemDef *config.FabricDef, dataDir string) (*Host, error) {
	nodeID, err := os.Hostname()
	if err != nil || nodeID == "" {
		nodeID = "node"
	}

	storeOptions, err := controlplane.BackendOptionsFromFabric(systemDef, dataDir)
	if err != nil {
		return nil, err
	}
	store, err := controlplane.NewStore(storeOptions)
	if err != nil {
		return nil, err
	}

	return &Host{
		SystemDef:     systemDef,
		DataDir:       dataDir,
		NodeID:        nodeID,
		ListenHost:    "0.0.0.0",
		AdvertiseHost: "127.0.0.1",
		StorageLayout: config.StorageLayout(systemDef, dataDir),
		ControlPlane:  store,
		Discovery:     controlplane.NewDiscovery(store),
		instances:     make(map[string]*hostedInstance),
	}, nil
}

func (h *Host) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return fmt.Errorf("node host already started")
	}
	hostCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.started = true
	h.startedAt = time.Now().UTC()
	h.mu.Unlock()
	started := false
	defer func() {
		if started {
			return
		}
		if cancel != nil {
			cancel()
		}
		if h.nodeIdentity != nil {
			h.nodeIdentity.Close()
			h.nodeIdentity = nil
		}
		h.mu.Lock()
		h.started = false
		h.startedAt = time.Time{}
		h.cancel = nil
		h.mu.Unlock()
	}()

	if h.Identity == nil {
		provider, err := identity.ProviderFromFabric(h.SystemDef, h.DataDir)
		if err != nil {
			return fmt.Errorf("create identity provider: %w", err)
		}
		h.Identity = provider
	}
	// In distributed mode the join-token authority lives on the control-plane
	// process; the node's local data dir holds no token state, so a local
	// authority call here would always fail. Skip it and let the remote
	// attestation in attestMembership do the validation.
	if !h.hasConfiguredControlPlane() {
		if h.Attestor == nil {
			h.Attestor = identity.NewLocalDevJoinTokenAuthority(h.DataDir, identity.TrustDomainFromFabric(h.SystemDef))
		}
		attestedNode, err := h.Attestor.AttestNode(hostCtx, h.nodeAttestation())
		if err != nil {
			return fmt.Errorf("attest node %q: %w", h.NodeID, err)
		}
		if attestedNode.NodeID != "" {
			h.NodeID = attestedNode.NodeID
		}
	}
	defs, err := h.selectedAgentDefs()
	if err != nil {
		return err
	}
	if err := h.ensureBundleFingerprints(defs); err != nil {
		return err
	}
	nodeIdentity, err := identity.NewManagedCertificate(hostCtx, h.Identity, h.nodeIdentityRequest(h.NodeID))
	if err != nil {
		return fmt.Errorf("issue node identity: %w", err)
	}
	h.nodeIdentity = nodeIdentity
	controlPlaneAddress, err := h.resolveControlPlaneAddress(hostCtx)
	if err != nil {
		return err
	}
	if controlPlaneAddress != "" {
		remoteClient := controlplane.NewRemoteClient(controlPlaneAddress, nodeIdentity.ClientTLS())
		h.ControlPlaneAddress = controlPlaneAddress
		h.Discovery = remoteClient
		h.Membership = remoteClient
	}
	if err := h.attestMembership(hostCtx); err != nil {
		if isTransientControlPlaneError(err) {
			slog.Warn("initial node attestation deferred", "node_id", h.NodeID, "error", err)
		} else {
			return err
		}
	}
	if h.Membership == nil {
		if err := h.ControlPlane.RegisterNode(hostCtx, h.nodeRecord(controlplane.NodeStateReady)); err != nil {
			return err
		}
	}
	for _, def := range defs {
		if err := h.startAgent(hostCtx, def); err != nil {
			h.Shutdown(context.Background())
			return err
		}
	}
	h.syncMembership(hostCtx, time.Now().UTC())

	h.heartbeatWg.Add(1)
	go h.runHeartbeatLoop(hostCtx)
	started = true
	return nil
}

func (h *Host) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		return nil
	}
	cancel := h.cancel
	h.started = false
	h.startedAt = time.Time{}
	h.cancel = nil
	instances := make([]*hostedInstance, 0, len(h.instances))
	for _, instance := range h.instances {
		instances = append(instances, instance)
	}
	h.instances = make(map[string]*hostedInstance)
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if h.nodeIdentity != nil {
		h.nodeIdentity.Close()
		h.nodeIdentity = nil
	}

	for _, instance := range instances {
		instance.cancel()
		if instance.server != nil {
			instance.server.Stop()
		}
		if instance.comm != nil {
			instance.comm.Close()
		}
		if instance.identity != nil {
			instance.identity.Close()
		}
		select {
		case <-instance.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if instance.membership != nil {
			if err := instance.membership.RemoveInstance(context.Background(), instance.instance.ID); err != nil {
				slog.Warn("remove instance failed", "instance_id", instance.instance.ID, "error", err)
			}
		} else if err := h.ControlPlane.RemoveInstance(context.Background(), instance.instance.ID); err != nil {
			slog.Warn("remove instance failed", "instance_id", instance.instance.ID, "error", err)
		}
	}

	if h.Membership != nil {
		if err := h.Membership.RegisterNode(context.Background(), h.nodeRecord(controlplane.NodeStateUnavailable)); err != nil {
			slog.Warn("mark node unavailable failed", "node_id", h.NodeID, "error", err)
		}
	} else if err := h.ControlPlane.RegisterNode(context.Background(), h.nodeRecord(controlplane.NodeStateUnavailable)); err != nil {
		slog.Warn("mark node unavailable failed", "node_id", h.NodeID, "error", err)
	}

	h.heartbeatWg.Wait()
	if closer, ok := h.ControlPlane.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			slog.Warn("control plane close failed", "error", err)
		}
	}
	return nil
}

func (h *Host) selectedAgentDefs() ([]runtime.AgentDefinition, error) {
	selected := make(map[string]bool)
	for _, name := range h.Agents {
		name = strings.TrimSpace(name)
		if name != "" {
			selected[name] = true
		}
	}

	var defs []runtime.AgentDefinition
	for _, def := range h.SystemDef.Agents {
		if def.Name == "conductor" {
			continue
		}
		if len(selected) > 0 && !selected[def.Name] {
			continue
		}
		defs = append(defs, def)
	}

	sort.Slice(defs, func(i, j int) bool {
		return defs[i].Name < defs[j].Name
	})

	if len(defs) == 0 {
		return nil, fmt.Errorf("no agent definitions selected for node host")
	}
	return defs, nil
}

func (h *Host) startAgent(ctx context.Context, def runtime.AgentDefinition) error {
	listenAddr := net.JoinHostPort(h.ListenHost, "0")
	instanceID := fmt.Sprintf("%s/%s", h.NodeID, def.Name)
	useAmbientNodeIdentity := !identity.SupportsArbitrarySubjects(h.Identity)

	var (
		managedIdentity *identity.ManagedCertificate
		serverTLS       *tls.Config
		clientTLS       *tls.Config
		err             error
	)
	if useAmbientNodeIdentity {
		if h.nodeIdentity == nil {
			return fmt.Errorf("node identity is not initialized")
		}
		serverTLS = h.nodeIdentity.ServerTLS()
		clientTLS = h.nodeIdentity.ClientTLS()
	} else {
		managedIdentity, err = identity.NewManagedCertificate(ctx, h.Identity, h.instanceIdentityRequest(def.Name, instanceID, net.JoinHostPort(h.AdvertiseHost, "0")))
		if err != nil {
			return fmt.Errorf("issue identity for %q: %w", def.Name, err)
		}
		serverTLS = managedIdentity.ServerTLS()
		clientTLS = managedIdentity.ClientTLS()
	}

	server, err := agentgrpc.NewServer(def.Name, listenAddr, 64, serverTLS)
	if err != nil {
		if managedIdentity != nil {
			managedIdentity.Close()
		}
		return fmt.Errorf("create gRPC server for %q: %w", def.Name, err)
	}

	agentDiscovery := h.Discovery
	if h.ControlPlaneAddress != "" {
		agentDiscovery = controlplane.NewRemoteClient(h.ControlPlaneAddress, clientTLS)
	}
	baseComm := agentgrpc.NewCommunicator(def.Name, server, agentDiscovery, clientTLS)
	comm := message.AnnotateSender(baseComm, h.NodeID, instanceID)
	storage := local.NewStorageWithLayout(h.StorageLayout, def.Name)
	workspace, err := runtime.OpenWorkspace(ctx, storage)
	if err != nil {
		baseComm.Close()
		server.Stop()
		if managedIdentity != nil {
			managedIdentity.Close()
		}
		return fmt.Errorf("materialize workspace for %q: %w", def.Name, err)
	}
	meter := local.NewMeter()
	specialKnowledge := ""
	if def.SpecialKnowledgeFile != "" {
		if data, err := storage.Read(ctx, runtime.TierAgent, "special_knowledge.md"); err == nil {
			specialKnowledge = string(data)
		}
	}
	systemPrompt := agent.BuildSystemPrompt(def, specialKnowledge, h.SystemDef.Agents)
	sendProgress := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		_ = comm.Send(context.Background(), &message.Message{
			From: def.Name,
			To:   "conductor",
			Type: message.TypeStatusUpdate,
			Parts: []message.Part{
				message.TextPart{Text: text},
			},
		})
	}

	generateFn := func(callCtx context.Context, input []*schema.Message) (*schema.Message, error) {
		var chatModel model.BaseChatModel
		if h.ModelFactory != nil {
			m, err := h.ModelFactory(callCtx, def.Model)
			if err != nil {
				return nil, err
			}
			chatModel = m
		} else {
			m, err := llm.NewChatModel(callCtx, def.Model, nil, h.SystemDef.Providers)
			if err != nil {
				return nil, err
			}
			chatModel = m
		}

		toolInfos := agent.BuildToolInfos(def.Tools)
		if len(toolInfos) > 0 {
			if tcm, ok := chatModel.(model.ToolCallingChatModel); ok {
				bound, err := tcm.WithTools(toolInfos)
				if err != nil {
					return nil, fmt.Errorf("bind tools for %q: %w", def.Name, err)
				}
				chatModel = bound
			}
		}

		metered := &llm.MeteredModel{
			Model:     chatModel,
			AgentName: def.Name,
			ModelID:   def.Model,
			Meter:     meter,
			DebugLog:  h.DebugLog,
			Options:   llm.ProviderOptions(def.Model, h.SystemDef.Providers),
			OnChunk: func(textSoFar string) {
				sendProgress(progressSnippet(textSoFar))
			},
			OnRetry: func(attempt, maxAttempts int, err error) {
				sendProgress(fmt.Sprintf("Retrying model call (%d/%d)...", attempt, maxAttempts))
			},
		}
		return metered.Generate(callCtx, input)
	}

	var toolExec *agent.ToolExecutor
	liveTools := agent.LiveTools(def.Tools)
	if len(liveTools) > 0 {
		toolExec = &agent.ToolExecutor{
			Tools:           liveTools,
			TierPaths:       workspace.TierPaths(),
			AgentName:       def.Name,
			MaxOutputTokens: llm.MaxOutputTokens(def.Model),
			ContextLimit:    llm.ContextLimit(def.Model),
			SyncWorkspace:   workspace.Sync,
		}
	}

	ag := &agent.Agent{
		Def:                def,
		Comm:               comm,
		Storage:            storage,
		Workspace:          workspace,
		Meter:              meter,
		Logger:             message.NewLogger(local.NewSharedAppender(storage)),
		Generate:           generateFn,
		SystemPrompt:       systemPrompt,
		ToolExecutor:       toolExec,
		PromptCacheEnabled: llm.HasPromptCaching(def.Model, h.SystemDef.Providers),
		OnProgress:         sendProgress,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- server.Serve()
	}()

	advertisedAddr, err := advertisedAddress(server.Addr(), h.AdvertiseHost)
	if err != nil {
		baseComm.Close()
		server.Stop()
		if managedIdentity != nil {
			managedIdentity.Close()
		}
		return err
	}
	instance := controlplane.AgentInstance{
		ID:              instanceID,
		Profile:         def.Name,
		NodeID:          h.NodeID,
		BundleDigest:    h.BundleDigest,
		ProfileDigest:   h.ProfileDigests[def.Name],
		Endpoint:        runtime.Endpoint{Address: advertisedAddr},
		State:           controlplane.InstanceStateReady,
		StartedAt:       time.Now().UTC(),
		LastHeartbeatAt: time.Now().UTC(),
	}
	var instanceMembership controlplane.MembershipWriter
	if h.ControlPlaneAddress != "" {
		instanceMembership = controlplane.NewRemoteClient(h.ControlPlaneAddress, clientTLS)
	}
	if h.Membership == nil {
		instanceMembership = nil
		if err := h.ControlPlane.RegisterInstance(ctx, instance); err != nil {
			baseComm.Close()
			server.Stop()
			if managedIdentity != nil {
				managedIdentity.Close()
			}
			return fmt.Errorf("register instance %q: %w", instanceID, err)
		}
	}

	agentCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- ag.Run(agentCtx)
	}()

	go func() {
		select {
		case err := <-serveErrCh:
			if err != nil {
				slog.Warn("node host agent server exited", "agent", def.Name, "error", err)
			}
			cancel()
		case <-agentCtx.Done():
		}
	}()

	h.mu.Lock()
	h.instances[instanceID] = &hostedInstance{
		instance:   instance,
		server:     server,
		comm:       baseComm,
		membership: instanceMembership,
		identity:   managedIdentity,
		cancel:     cancel,
		done:       done,
	}
	h.mu.Unlock()

	return nil
}

// hasConfiguredControlPlane reports whether the host has been told to talk
// to a remote control plane, either via an explicit address on the host or
// via the fabric definition. It must not consult the local store -- this
// runs during Start() before the remote client has been wired up, and is
// the signal that lets node attestation defer to the remote authority.
func (h *Host) hasConfiguredControlPlane() bool {
	if strings.TrimSpace(h.ControlPlaneAddress) != "" {
		return true
	}
	if h.SystemDef != nil && strings.TrimSpace(h.SystemDef.ControlPlane.API.Address) != "" {
		return true
	}
	return false
}

func (h *Host) resolveControlPlaneAddress(ctx context.Context) (string, error) {
	if h.ControlPlaneAddress != "" {
		return h.ControlPlaneAddress, nil
	}
	if address := strings.TrimSpace(h.SystemDef.ControlPlane.API.Address); address != "" {
		return address, nil
	}

	leader, ok, err := h.ControlPlane.GetLeader(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve control-plane leader: %w", err)
	}
	if !ok || leader.HolderAddress == "" {
		return "", nil
	}
	return leader.HolderAddress, nil
}

func (h *Host) runHeartbeatLoop(ctx context.Context) {
	defer h.heartbeatWg.Done()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.syncMembership(ctx, now)
		}
	}
}

func (h *Host) syncMembership(ctx context.Context, now time.Time) {
	if err := h.attestMembership(ctx); err != nil {
		slog.Warn("node attestation failed", "node_id", h.NodeID, "error", err)
		return
	}
	if h.Membership != nil {
		if err := h.Membership.RegisterNode(ctx, h.nodeRecord(controlplane.NodeStateReady)); err != nil {
			slog.Warn("node registration failed", "node_id", h.NodeID, "error", err)
		} else if err := h.Membership.HeartbeatNode(ctx, h.NodeID, now); err != nil {
			slog.Warn("node heartbeat failed", "node_id", h.NodeID, "error", err)
		}
	} else if err := h.ControlPlane.HeartbeatNode(ctx, h.NodeID, now); err != nil {
		slog.Warn("node heartbeat failed", "node_id", h.NodeID, "error", err)
	}

	h.mu.Lock()
	instances := make([]*hostedInstance, 0, len(h.instances))
	for _, hosted := range h.instances {
		instances = append(instances, hosted)
	}
	h.mu.Unlock()

	for _, hosted := range instances {
		if hosted.membership != nil {
			if err := hosted.membership.RegisterInstance(ctx, hosted.instance); err != nil {
				slog.Warn("instance registration failed", "instance_id", hosted.instance.ID, "error", err)
				continue
			}
			if err := hosted.membership.HeartbeatInstance(ctx, hosted.instance.ID, now); err != nil {
				slog.Warn("instance heartbeat failed", "instance_id", hosted.instance.ID, "error", err)
			}
		} else if err := h.ControlPlane.HeartbeatInstance(ctx, hosted.instance.ID, now); err != nil {
			slog.Warn("instance heartbeat failed", "instance_id", hosted.instance.ID, "error", err)
		}
	}
}

func (h *Host) attestMembership(ctx context.Context) error {
	attestor, ok := h.Membership.(interface {
		AttestNode(context.Context, identity.NodeAttestation) (identity.AttestedNode, error)
	})
	if !ok {
		return nil
	}
	attestedNode, err := attestor.AttestNode(ctx, h.nodeAttestation())
	if err != nil {
		return err
	}
	if attestedNode.NodeID != "" {
		h.NodeID = attestedNode.NodeID
	}
	return nil
}

func isTransientControlPlaneError(err error) bool {
	return status.Code(err) == codes.Unavailable
}

func (h *Host) instanceIdentityRequest(profile, instanceID, endpoint string) identity.IssueRequest {
	host, _, err := net.SplitHostPort(endpoint)
	request := identity.IssueRequest{
		Subject: identity.Subject{
			TrustDomain: identity.TrustDomainFromFabric(h.SystemDef),
			Fabric:      h.SystemDef.Fabric.Name,
			Kind:        identity.SubjectKindAgentInstance,
			Name:        profile,
			NodeID:      h.NodeID,
			Profile:     profile,
			InstanceID:  instanceID,
		},
		Principal: profile,
	}
	if err != nil {
		return request
	}
	if ip := net.ParseIP(host); ip != nil {
		request.IPAddresses = append(request.IPAddresses, ip)
	} else if host != "" {
		request.DNSNames = append(request.DNSNames, host)
	}
	return request
}

func (h *Host) nodeIdentityRequest(nodeID string) identity.IssueRequest {
	request := identity.IssueRequest{
		Subject: identity.Subject{
			TrustDomain: identity.TrustDomainFromFabric(h.SystemDef),
			Fabric:      h.SystemDef.Fabric.Name,
			Kind:        identity.SubjectKindNode,
			Name:        nodeID,
			NodeID:      nodeID,
		},
		Principal: nodeID,
	}
	if ip := net.ParseIP(h.AdvertiseHost); ip != nil {
		request.IPAddresses = append(request.IPAddresses, ip)
	} else if h.AdvertiseHost != "" {
		request.DNSNames = append(request.DNSNames, h.AdvertiseHost)
	}
	return request
}

func (h *Host) nodeAttestation() identity.NodeAttestation {
	request := h.Attestation
	if request.Type == "" {
		request.Type = identity.NodeJoinTokenAttestationType
	}
	if request.Claims == nil {
		request.Claims = make(map[string]string)
	}
	if request.Claims["node_id"] == "" {
		request.Claims["node_id"] = h.NodeID
	}
	if request.Claims["fabric"] == "" {
		request.Claims["fabric"] = h.SystemDef.Fabric.Name
	}
	if request.Measurements == nil {
		request.Measurements = make(map[string]string)
	}
	for key, value := range h.runtimeMeasurements() {
		if request.Measurements[key] == "" {
			request.Measurements[key] = value
		}
	}
	return request
}

func (h *Host) nodeRecord(state controlplane.NodeState) controlplane.Node {
	maxInstances := h.MaxInstances
	if maxInstances <= 0 {
		maxInstances = len(h.SystemDef.Agents) - 1
	}
	maxTasks := h.MaxTasks
	if maxTasks <= 0 {
		maxTasks = len(h.SystemDef.Agents) - 1
	}

	labels := map[string]string{
		"role": "node",
		"mode": "external",
	}
	for key, value := range h.NodeLabels {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		labels[key] = value
	}

	return controlplane.Node{
		ID:              h.NodeID,
		Address:         h.AdvertiseHost,
		State:           state,
		BundleDigest:    h.BundleDigest,
		ProfileDigests:  cloneStringMap(h.ProfileDigests),
		StartedAt:       h.startedAt,
		LastHeartbeatAt: time.Now().UTC(),
		Labels:          labels,
		Capacity: controlplane.NodeCapacity{
			MaxInstances: maxInstances,
			MaxTasks:     maxTasks,
		},
	}
}

func (h *Host) ensureBundleFingerprints(defs []runtime.AgentDefinition) error {
	if h.BundleDigest != "" && len(h.ProfileDigests) > 0 {
		return nil
	}

	fingerprint, err := config.ComputeBundleFingerprint(h.SystemDef)
	if err != nil {
		return fmt.Errorf("compute fabric bundle fingerprint: %w", err)
	}

	h.BundleDigest = fingerprint.BundleDigest
	h.ProfileDigests = make(map[string]string, len(defs))
	for _, def := range defs {
		if digest, ok := fingerprint.ProfileDigests[def.Name]; ok {
			h.ProfileDigests[def.Name] = digest
		}
	}
	return nil
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

func advertisedAddress(listenAddr, advertiseHost string) (string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("split listen address %q: %w", listenAddr, err)
	}
	host := advertiseHost
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func (h *Host) runtimeMeasurements() map[string]string {
	measurements := make(map[string]string)
	if h.BundleDigest != "" {
		measurements["bundle_digest"] = h.BundleDigest
	}
	executablePath, err := os.Executable()
	if err == nil {
		if digest, digestErr := identity.FileSHA256(executablePath); digestErr == nil {
			measurements["binary_sha256"] = digest
		}
	}
	return measurements
}

func progressSnippet(text string) string {
	snippet := text
	if len(snippet) > 80 {
		snippet = snippet[len(snippet)-80:]
	}
	return snippet
}
