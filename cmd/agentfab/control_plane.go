package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/controlplanesvc"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/spf13/cobra"
)

func controlPlaneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "control-plane",
		Short: "Control-plane service commands",
	}
	cmd.AddCommand(controlPlaneServeCmd())
	return cmd
}

func controlPlaneServeCmd() *cobra.Command {
	var (
		configFile string
		dataDir    string
		listenAddr string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control-plane API as an independent service",
		RunE: func(cmd *cobra.Command, args []string) error {
			systemDef, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := config.ResolvePathsRelativeToConfig(systemDef, configFile); err != nil {
				return err
			}
			if listenAddr == "" {
				listenAddr = strings.TrimSpace(systemDef.ControlPlane.API.ListenAddress)
			}
			if listenAddr == "" {
				listenAddr = strings.TrimSpace(systemDef.ControlPlane.API.Address)
			}
			if listenAddr == "" {
				listenAddr = ":50051"
			}

			verification, err := config.VerifySignedBundle(systemDef)
			if err != nil {
				return fmt.Errorf("verify active bundle: %w", err)
			}

			storeOptions, err := controlplane.BackendOptionsFromFabric(systemDef, dataDir)
			if err != nil {
				return fmt.Errorf("build control plane options: %w", err)
			}
			store, err := controlplane.NewStore(storeOptions)
			if err != nil {
				return fmt.Errorf("create control plane store: %w", err)
			}
			defer closeControlPlaneStore(store)

			provider, err := identity.ProviderFromFabric(systemDef, dataDir)
			if err != nil {
				return fmt.Errorf("create identity provider: %w", err)
			}

			serverIdentity, err := identity.NewManagedCertificate(
				context.Background(),
				provider,
				controlPlaneIdentityRequest(identity.TrustDomainFromFabric(systemDef), systemDef.Fabric.Name, systemDef.ControlPlane.API.Address, listenAddr),
			)
			if err != nil {
				return fmt.Errorf("issue control-plane identity: %w", err)
			}
			defer serverIdentity.Close()

			server, err := agentgrpc.NewServer("control-plane", listenAddr, 64, serverIdentity.ServerTLS())
			if err != nil {
				return fmt.Errorf("create control-plane gRPC server: %w", err)
			}
			server.SetControlPlaneService(controlplanesvc.New(controlplanesvc.Config{
				Store:                  store,
				Fabric:                 systemDef.Fabric.Name,
				ExpectedBundleDigest:   verification.BundleDigest,
				ExpectedProfileDigests: verification.ProfileDigests,
				Attestor:               identity.NewLocalDevJoinTokenAuthority(dataDir, identity.TrustDomainFromFabric(systemDef)),
			}))

			errCh := make(chan error, 1)
			go func() {
				errCh <- server.Serve()
			}()
			defer server.Stop()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			select {
			case sig := <-sigCh:
				_ = sig
				return nil
			case err := <-errCh:
				if err != nil {
					return fmt.Errorf("serve control-plane API: %w", err)
				}
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	cmd.Flags().StringVar(&dataDir, "data-dir", config.DefaultDataDir(), "Shared data directory")
	cmd.Flags().StringVar(&listenAddr, "listen", "", "Control-plane gRPC listen address")
	return cmd
}

func controlPlaneIdentityRequest(trustDomain, fabric, advertiseAddress, listenAddress string) identity.IssueRequest {
	request := identity.IssueRequest{
		Subject: identity.Subject{
			TrustDomain: trustDomain,
			Fabric:      fabric,
			Kind:        identity.SubjectKindControlPlane,
			Name:        "api",
		},
		Principal: "control-plane",
	}
	for _, endpoint := range []string{advertiseAddress, listenAddress} {
		host := controlPlaneHost(endpoint)
		if host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			request.IPAddresses = append(request.IPAddresses, ip)
			continue
		}
		request.DNSNames = append(request.DNSNames, host)
	}
	return request
}

func controlPlaneHost(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return endpoint
	}
	return host
}

func closeControlPlaneStore(store controlplane.Store) {
	closer, ok := store.(interface{ Close() error })
	if ok {
		_ = closer.Close()
	}
}
