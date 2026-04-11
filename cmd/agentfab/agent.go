package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/spf13/cobra"
)

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent management commands",
	}
	cmd.AddCommand(agentCompileCmd())
	return cmd
}

func agentCompileCmd() *cobra.Command {
	var (
		inputDir     string
		outputDir    string
		defaultModel string
		fabricName   string
		dryRun       bool
	)

	cmd := &cobra.Command{
		Use:   "compile",
		Short: "Compile agent .md descriptions into YAML definitions and agents.yaml",
		Long: `Reads .md files from the input directory (one per agent), generates
structured YAML agent definitions and copies the .md files as special
knowledge, then writes agents.yaml.

Each .md filename becomes the agent name (e.g., backend-dev.md becomes agent "backend-dev").
A conductor agent is auto-added if not present in the input.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fabricName == "" {
				cwd, _ := os.Getwd()
				fabricName = filepath.Base(cwd)
			}

			fmt.Printf("Reading agent descriptions from %s/\n", inputDir)
			descs, err := config.ReadAgentDescriptions(inputDir)
			if err != nil {
				return err
			}
			fmt.Printf("Found %d agent descriptions: ", len(descs))
			for i, d := range descs {
				if i > 0 {
					fmt.Print(", ")
				}
				fmt.Print(d.Name)
			}
			fmt.Println()

			results, err := config.CompileAgents(descs, defaultModel)
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Println("\n--- Dry run: generated definitions ---")
				for _, r := range results {
					fmt.Printf("\n## %s\n", r.Definition.Name)
					data, _ := config.MarshalAgentDef(r.Definition)
					fmt.Println(string(data))
				}
				return nil
			}

			if err := config.WriteCompiledAgents(outputDir, results); err != nil {
				return err
			}
			fmt.Printf("Wrote %d agent definitions to %s/\n", len(results), outputDir)

			td := &config.FabricDef{
				Fabric: config.FabricMeta{
					Name:    fabricName,
					Version: 1,
				},
				AgentsDir: outputDir,
			}
			if err := td.ResolveAgents(); err != nil {
				return fmt.Errorf("resolve agents: %w", err)
			}

			configPath := "agents.yaml"
			if err := config.WriteFabricDef(configPath, td); err != nil {
				return err
			}

			manifest, err := config.GenerateManifest(outputDir)
			if err != nil {
				return fmt.Errorf("generate manifest: %w", err)
			}
			if err := config.WriteManifest(config.ManifestPath(outputDir), manifest); err != nil {
				return fmt.Errorf("write manifest: %w", err)
			}

			fmt.Printf("Created %s with %d agents:\n", configPath, len(td.Agents))
			for _, a := range td.Agents {
				fmt.Printf("  - %s (%s)\n", a.Name, a.Model)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&inputDir, "input", "i", "./agents", "Directory of agent .md description files")
	cmd.Flags().StringVarP(&outputDir, "output", "o", "./agents", "Output directory for generated YAML + .md files")
	cmd.Flags().StringVar(&defaultModel, "model", "anthropic/claude-sonnet-4-5-20250929", "Default model for all agents")
	cmd.Flags().StringVar(&fabricName, "name", "", "Fabric name (defaults to directory name)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print generated YAML to stdout without writing files")

	return cmd
}
