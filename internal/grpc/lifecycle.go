package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	grpclib "google.golang.org/grpc"
	creds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ProcessLifecycle spawns each agent as a separate OS process.
type ProcessLifecycle struct {
	mu            sync.Mutex
	procs         map[string]*agentProcess
	binary        string // path to agentfab binary
	configFile    string // path to agents.yaml
	dataDir       string
	conductorAddr string // conductor's gRPC address
	discovery     *StaticDiscovery
	ca            *ClusterCA // cluster CA for issuing per-agent certs (nil = insecure)
	debug         bool       // pass --debug to spawned agents
}

type agentProcess struct {
	cmd     *exec.Cmd
	addr    string
	done    chan struct{}
	err     error
	logFile *os.File
}

func NewProcessLifecycle(configFile, dataDir, conductorAddr string, discovery *StaticDiscovery) *ProcessLifecycle {
	binary, _ := os.Executable()
	return &ProcessLifecycle{
		procs:         make(map[string]*agentProcess),
		binary:        binary,
		configFile:    configFile,
		dataDir:       dataDir,
		conductorAddr: conductorAddr,
		discovery:     discovery,
	}
}

// SetCA enables per-agent TLS certificate issuance during Spawn.
func (l *ProcessLifecycle) SetCA(ca *ClusterCA) {
	l.ca = ca
}

// SetDebug enables --debug on spawned agent processes.
func (l *ProcessLifecycle) SetDebug(enabled bool) {
	l.debug = enabled
}

func (l *ProcessLifecycle) Spawn(ctx context.Context, def runtime.AgentDefinition, _ func(ctx context.Context) error) error {
	l.mu.Lock()
	if _, exists := l.procs[def.Name]; exists {
		l.mu.Unlock()
		return fmt.Errorf("agent %q already running", def.Name)
	}
	l.mu.Unlock()

	if l.ca != nil {
		agentCert, certErr := l.ca.IssueCert(def.Name)
		if certErr != nil {
			return fmt.Errorf("issue cert for agent %q: %w", def.Name, certErr)
		}
		tlsDir := AgentTLSDir(l.dataDir, def.Name)
		if writeErr := WriteAgentCredentials(tlsDir, agentCert, l.ca.CACertPEM()); writeErr != nil {
			return fmt.Errorf("write TLS creds for agent %q: %w", def.Name, writeErr)
		}
	}

	port, err := allocFreePort()
	if err != nil {
		return fmt.Errorf("allocate port for agent %q: %w", def.Name, err)
	}
	listenAddr := fmt.Sprintf(":%d", port)

	// exec.Command (not CommandContext) so SIGTERM can be sent gracefully.
	cmdArgs := []string{"agent", "serve",
		"--name", def.Name,
		"--config", l.configFile,
		"--data-dir", l.dataDir,
		"--listen", listenAddr,
		"--conductor", l.conductorAddr,
	}
	if l.debug {
		cmdArgs = append(cmdArgs, "--debug")
	}
	cmd := exec.Command(l.binary, cmdArgs...)
	if l.ca != nil {
		cmd.Env = append(os.Environ(), "AGENTFAB_REQUIRE_TLS=1")
	}

	agentLogDir := filepath.Join(l.dataDir, "agents", def.Name)
	if mkErr := os.MkdirAll(agentLogDir, 0755); mkErr != nil {
		return fmt.Errorf("create agent log dir for %q: %w", def.Name, mkErr)
	}
	logFile, logErr := os.OpenFile(
		filepath.Join(agentLogDir, "process.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644,
	)
	if logErr != nil {
		return fmt.Errorf("create log file for agent %q: %w", def.Name, logErr)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start agent %q: %w", def.Name, err)
	}

	proc := &agentProcess{
		cmd:     cmd,
		addr:    fmt.Sprintf("localhost:%d", port),
		done:    make(chan struct{}),
		logFile: logFile,
	}

	go func() {
		defer close(proc.done)
		proc.err = cmd.Wait()
		logFile.Close()
	}()

	l.mu.Lock()
	l.procs[def.Name] = proc
	l.mu.Unlock()

	if l.discovery != nil {
		l.discovery.Register(ctx, def.Name, runtime.Endpoint{Address: proc.addr})
	}

	if err := l.waitForReady(ctx, proc.addr, def.Name, 30*time.Second, proc.done); err != nil {
		diagnostic := ""
		if logData, readErr := os.ReadFile(filepath.Join(agentLogDir, "process.log")); readErr == nil && len(logData) > 0 {
			lines := string(logData)
			if len(lines) > 500 {
				lines = lines[len(lines)-500:]
			}
			diagnostic = "\nAgent log tail:\n" + lines
		}
		l.Teardown(ctx, def.Name)
		return fmt.Errorf("agent %q failed to start: %w%s", def.Name, err, diagnostic)
	}

	slog.Info("agent process started", "name", def.Name, "addr", proc.addr, "pid", cmd.Process.Pid)
	return nil
}

func (l *ProcessLifecycle) Teardown(ctx context.Context, name string) error {
	l.mu.Lock()
	proc, ok := l.procs[name]
	l.mu.Unlock()

	if !ok {
		return fmt.Errorf("agent %q not found", name)
	}

	if proc.cmd != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
	}

	killAndWait := func() {
		if proc.cmd == nil || proc.cmd.Process == nil {
			return
		}
		_ = proc.cmd.Process.Kill()
		select {
		case <-proc.done:
		case <-time.After(2 * time.Second):
		}
	}

	select {
	case <-proc.done:
	case <-ctx.Done():
		killAndWait()
	case <-time.After(10 * time.Second):
		killAndWait()
	}

	l.mu.Lock()
	delete(l.procs, name)
	l.mu.Unlock()

	if l.discovery != nil {
		_ = l.discovery.Deregister(context.Background(), name)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return proc.err
}

func (l *ProcessLifecycle) TeardownAll(ctx context.Context) error {
	l.mu.Lock()
	names := make([]string, 0, len(l.procs))
	for name := range l.procs {
		names = append(names, name)
	}
	l.mu.Unlock()

	type teardownResult struct {
		name string
		err  error
	}
	results := make(chan teardownResult, len(names))
	for _, name := range names {
		go func(name string) {
			results <- teardownResult{name: name, err: l.Teardown(ctx, name)}
		}(name)
	}

	var firstErr error
	for range names {
		res := <-results
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
	}
	return firstErr
}

func (l *ProcessLifecycle) Wait(_ context.Context, name string) error {
	l.mu.Lock()
	proc, ok := l.procs[name]
	l.mu.Unlock()

	if !ok {
		return fmt.Errorf("agent %q not found", name)
	}

	<-proc.done
	return proc.err
}

func (l *ProcessLifecycle) Respawn(ctx context.Context, def runtime.AgentDefinition) error {
	l.Teardown(ctx, def.Name)
	return l.Spawn(ctx, def, nil)
}

func (l *ProcessLifecycle) waitForReady(ctx context.Context, addr, name string, timeout time.Duration, processDone <-chan struct{}) error {
	deadline := time.Now().Add(timeout)

	var dialCreds grpclib.DialOption
	if l.ca != nil {
		pollerCert, certErr := l.ca.IssueCert("conductor")
		if certErr != nil {
			return fmt.Errorf("issue poller cert: %w", certErr)
		}
		dialCreds = grpclib.WithTransportCredentials(
			creds.NewTLS(ClientTLSConfig(pollerCert, l.ca.Pool())),
		)
	} else {
		dialCreds = grpclib.WithTransportCredentials(insecure.NewCredentials())
	}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-processDone:
			return fmt.Errorf("process exited before becoming ready")
		default:
		}

		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		conn.Close()

		grpcConn, err := grpclib.NewClient(addr, dialCreds)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		client := pb.NewAgentServiceClient(grpcConn)
		hbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := client.Heartbeat(hbCtx, &pb.HeartbeatRequest{})
		cancel()
		grpcConn.Close()

		if err == nil && resp.Name == name && resp.Status == "ready" {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for agent %q at %s", name, addr)
}

// PeersFilePath returns the canonical path for the peers.json file.
func PeersFilePath(dataDir string) string {
	return filepath.Join(dataDir, "shared", "peers.json")
}

// WritePeers writes all known agent addresses to a peers.json file so that
// agents spawned in distributed mode can discover each other. This should be
// called after all agents are spawned.
func (l *ProcessLifecycle) WritePeers() error {
	l.mu.Lock()
	peers := make(map[string]string, len(l.procs))
	for name, proc := range l.procs {
		peers[name] = proc.addr
	}
	l.mu.Unlock()

	// Include the conductor.
	if l.conductorAddr != "" {
		peers["conductor"] = l.conductorAddr
	}

	data, err := json.Marshal(peers)
	if err != nil {
		return fmt.Errorf("marshal peers: %w", err)
	}

	path := PeersFilePath(l.dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create peers dir: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func allocFreePort() (int, error) {
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	port := lis.Addr().(*net.TCPAddr).Port
	lis.Close()
	return port, nil
}
