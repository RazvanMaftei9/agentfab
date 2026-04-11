package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/razvanmaftei/agentfab/internal/nodehost"
	"github.com/spf13/cobra"
)

func nodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "External node runtime commands",
	}
	cmd.AddCommand(nodeTokenCmd())
	cmd.AddCommand(nodeServeCmd())
	return cmd
}

func nodeServeCmd() *cobra.Command {
	var (
		configFile          string
		dataDir             string
		nodeID              string
		listenHost          string
		advertiseHost       string
		controlPlaneAddress string
		nodeLabels          string
		maxInstances        int
		maxTasks            int
		agents              string
		enrollmentToken     string
		enrollmentTokenFile string
		skipVerify          bool
		debug               bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an external node host that serves one or more agent instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			systemDef, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := config.ResolvePathsRelativeToConfig(systemDef, configFile); err != nil {
				return err
			}
			var verification config.BundleVerificationResult
			if !skipVerify {
				verification, err = config.VerifySignedBundle(systemDef)
				if err != nil {
					return fmt.Errorf("verify agent bundle: %w", err)
				}
			}

			host, err := nodehost.New(systemDef, dataDir)
			if err != nil {
				return fmt.Errorf("create node host: %w", err)
			}
			if verification.BundleDigest != "" {
				host.BundleDigest = verification.BundleDigest
				host.ProfileDigests = verification.ProfileDigests
			}
			if nodeID != "" {
				host.NodeID = nodeID
			}
			if listenHost != "" {
				host.ListenHost = listenHost
			}
			if advertiseHost != "" {
				host.AdvertiseHost = advertiseHost
			}
			if controlPlaneAddress != "" {
				host.ControlPlaneAddress = controlPlaneAddress
			}
			if nodeLabels != "" {
				labels, err := parseNodeLabels(nodeLabels)
				if err != nil {
					return err
				}
				host.NodeLabels = labels
			}
			host.MaxInstances = maxInstances
			host.MaxTasks = maxTasks
			if agents != "" {
				host.Agents = strings.Split(agents, ",")
			}
			token, err := resolveEnrollmentToken(enrollmentToken, enrollmentTokenFile)
			if err != nil {
				return err
			}
			host.Attestation = identity.NodeAttestation{
				Type:  identity.NodeJoinTokenAttestationType,
				Token: token,
			}

			if debug {
				debugDir := dataDir + "/debug"
				debugStore, err := llm.NewDebugStore(debugDir)
				if err != nil {
					return fmt.Errorf("create debug store: %w", err)
				}
				defer debugStore.Close()
				host.DebugLog = debugStore
			}

			host.ModelFactory = func(ctx context.Context, modelID string) (model.ChatModel, error) {
				return llm.NewChatModel(ctx, modelID, nil, systemDef.Providers)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if err := host.Start(ctx); err != nil {
				return fmt.Errorf("start node host: %w", err)
			}
			defer host.Shutdown(context.Background())

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			<-sigCh
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	cmd.Flags().StringVar(&dataDir, "data-dir", config.DefaultDataDir(), "Shared data directory")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "Stable node identifier (defaults to hostname)")
	cmd.Flags().StringVar(&listenHost, "listen-host", "0.0.0.0", "Host/IP to bind agent gRPC listeners on")
	cmd.Flags().StringVar(&advertiseHost, "advertise-host", "", "Host/IP other nodes should use to reach this node")
	cmd.Flags().StringVar(&controlPlaneAddress, "control-plane-address", "", "Reachable address of the control-plane API")
	cmd.Flags().StringVar(&nodeLabels, "labels", "", "Comma-separated node labels in key=value form")
	cmd.Flags().IntVar(&maxInstances, "max-instances", 0, "Maximum agent instances the node should advertise (default: number of non-conductor agents)")
	cmd.Flags().IntVar(&maxTasks, "max-tasks", 0, "Maximum concurrent tasks the node should advertise (default: number of non-conductor agents)")
	cmd.Flags().StringVar(&agents, "agents", "", "Comma-separated agent profiles to host (default: all non-conductor agents)")
	cmd.Flags().StringVar(&enrollmentToken, "enrollment-token", "", "Node enrollment token")
	cmd.Flags().StringVar(&enrollmentTokenFile, "enrollment-token-file", "", "Path to a file containing the node enrollment token")
	cmd.Flags().BoolVar(&skipVerify, "skip-verify", false, "Skip agent bundle and manifest integrity verification")
	cmd.Flags().BoolVar(&debug, "debug", false, "Log full LLM requests/responses as JSONL")
	return cmd
}

func nodeTokenCmd() *cobra.Command {
	var (
		configFile  string
		dataDir     string
		nodeID      string
		description string
		ttl         time.Duration
		reusable    bool
		bindBundle  bool
		bindBinary  bool
	)

	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage node enrollment tokens",
	}
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a node enrollment token for an external node",
		RunE: func(cmd *cobra.Command, args []string) error {
			systemDef, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := config.ResolvePathsRelativeToConfig(systemDef, configFile); err != nil {
				return err
			}

			expectedMeasurements := make(map[string]string)
			if bindBundle {
				verification, err := config.VerifySignedBundle(systemDef)
				if err != nil {
					return fmt.Errorf("verify bundle before issuing token: %w", err)
				}
				if verification.BundleDigest == "" {
					return fmt.Errorf("bundle digest is required for measured token issuance")
				}
				expectedMeasurements["bundle_digest"] = verification.BundleDigest
			}
			if bindBinary {
				executablePath, err := os.Executable()
				if err != nil {
					return fmt.Errorf("resolve executable path: %w", err)
				}
				digest, err := identity.FileSHA256(executablePath)
				if err != nil {
					return fmt.Errorf("compute executable digest: %w", err)
				}
				expectedMeasurements["binary_sha256"] = digest
			}
			authority := identity.NewLocalDevJoinTokenAuthority(dataDir, identity.TrustDomainFromFabric(systemDef))
			token, err := authority.IssueNodeToken(context.Background(), identity.NodeTokenRequest{
				Fabric:               systemDef.Fabric.Name,
				NodeID:               nodeID,
				Description:          description,
				ExpiresAt:            time.Now().Add(ttl),
				Reusable:             reusable,
				ExpectedMeasurements: expectedMeasurements,
			})
			if err != nil {
				return fmt.Errorf("issue node enrollment token: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Node enrollment token created\n")
			fmt.Fprintf(cmd.OutOrStdout(), "  Token ID: %s\n", token.ID)
			if token.NodeID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  Node ID: %s\n", token.NodeID)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  Fabric: %s\n", token.Fabric)
			fmt.Fprintf(cmd.OutOrStdout(), "  Expires: %s\n", token.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintf(cmd.OutOrStdout(), "  Reusable: %t\n", token.Reusable)
			for key, value := range token.ExpectedMeasurements {
				fmt.Fprintf(cmd.OutOrStdout(), "  Measurement %s: %s\n", key, value)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  Token: %s\n", token.Value)
			fmt.Fprintf(cmd.OutOrStdout(), "\nUse it with:\n")
			fmt.Fprintf(cmd.OutOrStdout(), "  agentfab node serve --config %s --data-dir %s --enrollment-token %s\n", configFile, dataDir, token.Value)
			return nil
		},
	}
	createCmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	createCmd.Flags().StringVar(&dataDir, "data-dir", config.DefaultDataDir(), "Shared data directory")
	createCmd.Flags().StringVar(&nodeID, "node-id", "", "Bind the token to a specific node ID")
	createCmd.Flags().StringVar(&description, "description", "", "Optional description for the token")
	createCmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "Token lifetime")
	createCmd.Flags().BoolVar(&reusable, "reusable", true, "Allow the token to be reused by the bound node")
	createCmd.Flags().BoolVar(&bindBundle, "bind-bundle", true, "Bind the token to the active fabric bundle digest")
	createCmd.Flags().BoolVar(&bindBinary, "bind-binary", true, "Bind the token to the current agentfab binary digest")
	cmd.AddCommand(createCmd)
	return cmd
}

func resolveEnrollmentToken(tokenValue, tokenFile string) (string, error) {
	if tokenValue != "" && tokenFile != "" {
		return "", fmt.Errorf("use either --enrollment-token or --enrollment-token-file")
	}
	if tokenFile != "" {
		data, err := os.ReadFile(filepath.Clean(tokenFile))
		if err != nil {
			return "", fmt.Errorf("read enrollment token file: %w", err)
		}
		tokenValue = strings.TrimSpace(string(data))
	}
	tokenValue = strings.TrimSpace(tokenValue)
	if tokenValue == "" {
		return "", fmt.Errorf("node enrollment token is required")
	}
	return tokenValue, nil
}

func parseNodeLabels(raw string) (map[string]string, error) {
	labels := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid node label %q: expected key=value", part)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return nil, fmt.Errorf("invalid node label %q: expected non-empty key and value", part)
		}
		labels[key] = value
	}
	return labels, nil
}
