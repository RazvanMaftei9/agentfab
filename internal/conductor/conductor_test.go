package conductor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/controlplanesvc"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/nodehost"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

// mockChatModel implements model.ChatModel for testing.
type mockChatModel struct {
	generateFn func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

func (m *mockChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.generateFn(ctx, input, opts...)
}

func (m *mockChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.generateFn(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()
	return sr, nil
}

func (m *mockChatModel) BindTools(tools []*schema.ToolInfo) error {
	return nil
}

func TestConductorStartAndShutdown(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "mock response",
				}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Verify agents are registered.
	states := c.AgentStates(ctx)
	if len(states) != 4 {
		t.Errorf("agents: got %d, want 4", len(states))
	}

	// Shutdown.
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestConductorUsesIndependentControlPlaneService(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")
	systemDef.ControlPlane.API.Address = "127.0.0.1:50051"

	store := controlplane.NewMemoryStore(systemDef.Fabric.Name)
	provider, err := identity.NewSharedLocalDevProvider(dir, "agentfab.local")
	if err != nil {
		t.Fatalf("NewSharedLocalDevProvider: %v", err)
	}
	serverIdentity, err := provider.IssueCertificate(context.Background(), identity.IssueRequest{
		Subject: identity.Subject{
			TrustDomain: "agentfab.local",
			Fabric:      systemDef.Fabric.Name,
			Kind:        identity.SubjectKindControlPlane,
			Name:        "api",
		},
		Principal: "control-plane",
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
	})
	if err != nil {
		t.Fatalf("IssueCertificate control-plane: %v", err)
	}

	server, err := agentgrpc.NewServer("control-plane", "127.0.0.1:0", 16, serverIdentity.ServerTLS)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server.SetControlPlaneService(controlplanesvc.New(controlplanesvc.Config{
		Store:    store,
		Fabric:   systemDef.Fabric.Name,
		Attestor: identity.NewLocalDevJoinTokenAuthority(dir, "agentfab.local"),
	}))
	go func() {
		_ = server.Serve()
	}()
	defer server.Stop()

	systemDef.ControlPlane.API.Address = server.Addr()

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock response"}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil, WithExternalAgents(), WithConductorListenAddr("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if err := c.startControlPlane(ctx); err != nil {
		t.Fatalf("startControlPlane: %v", err)
	}
	defer c.Shutdown(ctx)

	if _, ok := c.ControlPlane.(*controlplane.RemoteClient); !ok {
		t.Fatalf("control plane client = %T, want *controlplane.RemoteClient", c.ControlPlane)
	}

	leader, ok, err := store.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader: %v", err)
	}
	if !ok {
		t.Fatal("expected leader lease in external control plane store")
	}
	if leader.HolderID != "conductor" {
		t.Fatalf("leader holder = %q, want conductor", leader.HolderID)
	}
	if leader.HolderAddress == "127.0.0.1:0" {
		t.Fatal("leader holder address should use the bound conductor port, not 127.0.0.1:0")
	}

	node, ok, err := store.GetNode(ctx, "conductor")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !ok {
		t.Fatal("expected conductor node registration in external control plane store")
	}
	if node.BundleDigest == "" {
		t.Fatal("expected conductor node registration to include bundle digest")
	}
	if node.Address == "127.0.0.1:0" {
		t.Fatal("conductor node address should use the bound conductor port, not 127.0.0.1:0")
	}
}

func TestConductorShutdownCancelsBackgroundWork(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock response"}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil)
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}

	released := make(chan struct{})
	c.knowledgeWg.Add(1)
	go func() {
		defer c.knowledgeWg.Done()
		<-c.backgroundCtx.Done()
		close(released)
	}()

	shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(shutCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case <-released:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("background work was not canceled on shutdown")
	}
}

func TestActualConductorAddressUsesAdvertiseHostHint(t *testing.T) {
	t.Setenv("AGENTFAB_ADVERTISE_HOST", "10.244.2.5")

	got := actualConductorAddress("[::]:50050", ":50050", "")
	if got != "10.244.2.5:50050" {
		t.Fatalf("actualConductorAddress = %q, want %q", got, "10.244.2.5:50050")
	}
}

func TestActualConductorAddressUsesExplicitAdvertiseAddress(t *testing.T) {
	got := actualConductorAddress("0.0.0.0:50050", ":50050", "control-plane.agentfab.svc.cluster.local")
	if got != "control-plane.agentfab.svc.cluster.local:50050" {
		t.Fatalf("actualConductorAddress = %q, want %q", got, "control-plane.agentfab.svc.cluster.local:50050")
	}
}

func TestConductorHandleRequest(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				// First call is disambiguation.
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"clear": true}`,
					}, nil
				}
				// Second call is decomposition by conductor.
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design the system", "depends_on": []}]}`,
					}, nil
				}
				// Subsequent calls are agent task execution.
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Task completed successfully.",
				}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	result, err := c.HandleRequest(ctx, "Design a REST API")
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}

	if result == "" {
		t.Fatal("expected non-empty result")
	}

	// Verify conductor-written summary artifacts.
	// Per-request traces live under shared/artifacts/_requests/{requestID}/.
	// Material artifacts live under shared/artifacts/{agentName}/ (global).
	requestDirs, _ := filepath.Glob(filepath.Join(dir, "shared", "artifacts", "_requests", "*"))
	if len(requestDirs) == 0 {
		t.Fatal("expected _requests artifact directory to be created")
	}
	requestDir := requestDirs[0]
	resultsFile := filepath.Join(requestDir, "results.md")
	if _, err := os.Stat(resultsFile); os.IsNotExist(err) {
		t.Error("expected results.md artifact file")
	}
	taskFile := filepath.Join(requestDir, "t1.md")
	if _, err := os.Stat(taskFile); os.IsNotExist(err) {
		t.Error("expected t1.md artifact file")
	}

	// Verify metering recorded calls (Bug 4).
	states := c.AgentStates(ctx)
	totalCalls := int64(0)
	for _, s := range states {
		totalCalls += s.TotalCalls
	}
	if totalCalls == 0 {
		t.Error("expected metering to record at least one LLM call")
	}

	t.Logf("Result: %s", result)
}

func TestConductorHandleRequestPersistsControlPlaneState(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")
	store := controlplane.NewMemoryStore(systemDef.Fabric.Name)

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"clear": true}`,
					}, nil
				}
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design the system", "depends_on": []}]}`,
					}, nil
				}
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Task completed successfully.",
				}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil, WithControlPlaneStore(store))
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	if _, err := c.HandleRequest(ctx, "Design a REST API"); err != nil {
		t.Fatalf("handle request: %v", err)
	}

	requests, err := store.ListRequests(ctx)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests: got %d, want 1", len(requests))
	}
	if requests[0].State != controlplane.RequestStateCompleted {
		t.Fatalf("request state: got %q, want %q", requests[0].State, controlplane.RequestStateCompleted)
	}

	tasks, err := store.ListTasks(ctx, requests[0].ID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks: got %d, want 1", len(tasks))
	}
	if tasks[0].Profile != "architect" {
		t.Fatalf("task profile: got %q, want architect", tasks[0].Profile)
	}
	if tasks[0].Status != string(taskgraph.StatusCompleted) {
		t.Fatalf("task status: got %q, want %q", tasks[0].Status, taskgraph.StatusCompleted)
	}
}

func TestConductorStartRegistersControlPlaneNodeAndLeader(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")
	store := controlplane.NewMemoryStore(systemDef.Fabric.Name)

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil, WithControlPlaneStore(store), WithConductorID("conductor-test"))
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	node, ok, err := store.GetNode(ctx, "conductor-test")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !ok {
		t.Fatal("expected conductor node to be registered")
	}
	if node.State != controlplane.NodeStateReady {
		t.Fatalf("node state: got %q, want %q", node.State, controlplane.NodeStateReady)
	}
	if node.Labels["role"] != "conductor" {
		t.Fatalf("node role label: got %q, want conductor", node.Labels["role"])
	}

	lease, ok, err := store.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader: %v", err)
	}
	if !ok {
		t.Fatal("expected leader lease to be acquired")
	}
	if lease.HolderID != "conductor-test" {
		t.Fatalf("leader holder: got %q, want conductor-test", lease.HolderID)
	}
}

func TestConductorShutdownReleasesControlPlaneLeader(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")
	store := controlplane.NewMemoryStore(systemDef.Fabric.Name)

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil, WithControlPlaneStore(store), WithConductorID("conductor-test"))
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if _, ok, err := store.GetLeader(ctx); err != nil {
		t.Fatalf("GetLeader: %v", err)
	} else if ok {
		t.Fatal("expected leader lease to be released")
	}

	node, ok, err := store.GetNode(ctx, "conductor-test")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !ok {
		t.Fatal("expected conductor node to remain registered")
	}
	if node.State != controlplane.NodeStateUnavailable {
		t.Fatalf("node state: got %q, want %q", node.State, controlplane.NodeStateUnavailable)
	}
}

func TestConductorDefaultControlPlanePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"clear": true}`,
					}, nil
				}
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design the system", "depends_on": []}]}`,
					}, nil
				}
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Task completed successfully.",
				}, nil
			},
		}, nil
	}

	first, err := New(systemDef, dir, mockFactory, nil, WithConductorID("conductor-a"))
	if err != nil {
		t.Fatalf("new first conductor: %v", err)
	}

	ctx := context.Background()
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first conductor: %v", err)
	}

	if _, err := first.HandleRequest(ctx, "Design a REST API"); err != nil {
		t.Fatalf("handle request: %v", err)
	}

	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown first conductor: %v", err)
	}

	second, err := New(systemDef, dir, mockFactory, nil, WithConductorID("conductor-b"))
	if err != nil {
		t.Fatalf("new second conductor: %v", err)
	}
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start second conductor: %v", err)
	}
	defer second.Shutdown(ctx)

	store, ok := second.ControlPlane.(*controlplane.FileStore)
	if !ok {
		t.Fatalf("control plane type = %T, want *controlplane.FileStore", second.ControlPlane)
	}

	requests, err := store.ListRequests(ctx)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if requests[0].State != controlplane.RequestStateCompleted {
		t.Fatalf("request state = %q, want %q", requests[0].State, controlplane.RequestStateCompleted)
	}

	lease, ok, err := store.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader: %v", err)
	}
	if !ok {
		t.Fatal("expected leader to be present after restart")
	}
	if lease.HolderID != "conductor-b" {
		t.Fatalf("leader holder = %q, want conductor-b", lease.HolderID)
	}
	if lease.Epoch != 2 {
		t.Fatalf("leader epoch = %d, want 2", lease.Epoch)
	}
}

func TestConductorStartupInterruptsRecoveredRunningRequests(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	store, err := controlplane.NewFileStore(dir, systemDef.Fabric.Name)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	ctx := context.Background()
	if err := store.UpsertRequest(ctx, controlplane.RequestRecord{
		ID:           "req-recover",
		State:        controlplane.RequestStateRunning,
		UserRequest:  "Recover this request",
		GraphVersion: 1,
		LeaderID:     "conductor-old",
	}); err != nil {
		t.Fatalf("UpsertRequest: %v", err)
	}
	if err := store.UpsertTask(ctx, controlplane.TaskRecord{
		RequestID: "req-recover",
		TaskID:    "task-1",
		Profile:   "architect",
		Status:    string(taskgraph.StatusRunning),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	lease, acquired, err := store.AcquireTaskLease(ctx, controlplane.TaskLease{
		RequestID: "req-recover",
		TaskID:    "task-1",
		Profile:   "architect",
		OwnerID:   "conductor-old",
	}, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireTaskLease: %v", err)
	}
	if !acquired {
		t.Fatal("expected stale task lease to be created")
	}
	_ = lease

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, err := New(systemDef, dir, mockFactory, nil, WithConductorID("conductor-new"))
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start conductor: %v", err)
	}
	defer c.Shutdown(ctx)

	fileStore, ok := c.ControlPlane.(*controlplane.FileStore)
	if !ok {
		t.Fatalf("control plane type = %T, want *controlplane.FileStore", c.ControlPlane)
	}

	request, ok, err := fileStore.GetRequest(ctx, "req-recover")
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if !ok {
		t.Fatal("expected recovered request to remain present")
	}
	if request.State != controlplane.RequestStateInterrupted {
		t.Fatalf("request state = %q, want %q", request.State, controlplane.RequestStateInterrupted)
	}
	if request.LeaderID != "conductor-new" {
		t.Fatalf("request leader = %q, want conductor-new", request.LeaderID)
	}

	tasks, err := fileStore.ListTasks(ctx, "req-recover")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}
	if tasks[0].Status != "interrupted" {
		t.Fatalf("task status = %q, want interrupted", tasks[0].Status)
	}

	if _, ok, err := fileStore.GetTaskLease(ctx, "req-recover", "task-1"); err != nil {
		t.Fatalf("GetTaskLease: %v", err)
	} else if ok {
		t.Fatal("expected stale task lease to be released")
	}
}

func TestConductorHandleRequestWithExternalNodeHost(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	nodeFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Task completed successfully.",
				}, nil
			},
		}, nil
	}

	host, err := nodehost.New(systemDef, dir)
	if err != nil {
		t.Fatalf("new node host: %v", err)
	}
	host.NodeID = "node-a"
	host.ListenHost = "127.0.0.1"
	host.AdvertiseHost = "127.0.0.1"
	host.ModelFactory = nodeFactory
	authority := identity.NewLocalDevJoinTokenAuthority(dir, "agentfab.local")
	token, err := authority.IssueNodeToken(context.Background(), identity.NodeTokenRequest{
		Fabric:    systemDef.Fabric.Name,
		NodeID:    host.NodeID,
		ExpiresAt: time.Now().Add(time.Hour),
		Reusable:  true,
	})
	if err != nil {
		t.Fatalf("issue node token: %v", err)
	}
	host.Attestation = identity.NodeAttestation{
		Type:  identity.NodeJoinTokenAttestationType,
		Token: token.Value,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	conductorAddr := listener.Addr().String()
	listener.Close()
	host.ControlPlaneAddress = conductorAddr

	ctx := context.Background()
	if err := host.Start(ctx); err != nil {
		t.Fatalf("start node host: %v", err)
	}
	defer host.Shutdown(ctx)

	callCount := 0
	conductorFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				if callCount == 1 {
					return &schema.Message{Role: schema.Assistant, Content: `{"clear": true}`}, nil
				}
				if callCount == 2 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"actionable": true, "tasks": [{"id": "t1", "agent": "architect", "description": "Design the system", "depends_on": []}]}`,
					}, nil
				}
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "Task completed successfully.",
				}, nil
			},
		}, nil
	}

	c, err := New(
		systemDef,
		dir,
		conductorFactory,
		nil,
		WithExternalAgents(),
		WithConductorListenAddr(conductorAddr),
		WithConductorID("conductor-external"),
	)
	if err != nil {
		t.Fatalf("new conductor: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start conductor: %v", err)
	}
	defer c.Shutdown(ctx)

	result, err := c.HandleRequest(ctx, "Design a REST API")
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestConductorAgentChatInfo(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	entries := c.AgentChatInfo()
	if len(entries) == 0 {
		t.Fatal("expected at least one entry")
	}

	// Conductor should be first.
	if entries[0].Name != "conductor" {
		t.Errorf("first entry: got %q, want conductor", entries[0].Name)
	}
	if entries[0].Status != "conductor" {
		t.Errorf("conductor status: got %q, want conductor", entries[0].Status)
	}

	// All other agents should be idle (no active execution).
	for _, e := range entries[1:] {
		if e.Status != "idle" {
			t.Errorf("agent %q: got status %q, want idle", e.Name, e.Status)
		}
	}
}

func TestConductorChat(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "chat response"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	resp, err := c.Chat(ctx, ChatRequest{
		AgentName: "architect",
		Message:   "test question",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Response != "chat response" {
		t.Errorf("response: got %q", resp.Response)
	}
}

func TestConductorRestructureGraphNoExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	// RestructureGraph should fail when no execution is active.
	err := c.RestructureGraph(ctx, "test request", "test amendment")
	if err == nil {
		t.Fatal("expected error when no active execution")
	}
	if err.Error() != "no active execution" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConductorHandleRequestScreened(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	callCount := 0
	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				callCount++
				// First call is disambiguation — return clear.
				if callCount == 1 {
					return &schema.Message{
						Role:    schema.Assistant,
						Content: `{"clear": true}`,
					}, nil
				}
				// Second call is decompose — return non-actionable.
				return &schema.Message{
					Role:    schema.Assistant,
					Content: `{"actionable": false, "message": "Hey! What can I help you build?"}`,
				}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	result, err := c.HandleRequest(ctx, "Hey/")
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}

	if result != "" {
		t.Errorf("expected empty result for screened request, got: %q", result)
	}

	// Disambiguation (1 call) + decompose (1 call) = 2 calls total.
	if callCount != 2 {
		t.Errorf("expected exactly 2 LLM calls (disambiguate + decompose), got %d", callCount)
	}
}

func TestConductorCancelExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	type requestResult struct {
		result string
		err    error
	}

	resultCh := make(chan requestResult, 1)
	go func() {
		result, err := c.HandleRequest(ctx, "Build something")
		resultCh <- requestResult{result: result, err: err}
	}()

	active := false
	for !active {
		c.mu.RLock()
		active = c.activeReqCancel != nil
		c.mu.RUnlock()
		if active {
			break
		}

		select {
		case res := <-resultCh:
			t.Fatalf("request exited before becoming cancellable: result=%q err=%v", res.result, res.err)
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for active request")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Cancel execution.
	if !c.CancelExecution() {
		t.Fatal("CancelExecution should return true during execution")
	}

	select {
	case res := <-resultCh:
		if res.err != ErrRequestCancelled {
			t.Fatalf("expected ErrRequestCancelled, got: %v (result=%q)", res.err, res.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for HandleRequest to return after cancel")
	}
}

func TestConductorCancelNoExecution(t *testing.T) {
	dir := t.TempDir()
	systemDef := config.DefaultFabricDef("test-fabric")

	mockFactory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
		return &mockChatModel{
			generateFn: func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
				return &schema.Message{Role: schema.Assistant, Content: "mock"}, nil
			},
		}, nil
	}

	c, _ := New(systemDef, dir, mockFactory, nil)
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Shutdown(ctx)

	if c.CancelExecution() {
		t.Error("CancelExecution should return false when idle")
	}
}
