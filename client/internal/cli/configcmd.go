// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
)

func newConfigCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the resolved client configuration",
	}
	cmd.AddCommand(newConfigShowCmd(g))
	return cmd
}

type configView struct {
	ConfigFile    string `json:"config_file"`
	DockerContext string `json:"docker_context"`
	ManagerHost   string `json:"manager_host"`
	AgentPort     int    `json:"agent_port"`
	AddrMode      string `json:"addr_mode"`
	AuthMode      string `json:"auth_mode"`
	CA            string `json:"ca,omitempty"`
	Cert          string `json:"cert,omitempty"`
	Key           string `json:"key,omitempty"`
	ServerName    string `json:"server_name,omitempty"`
	Insecure      bool   `json:"insecure"`
	AgentSecret   string `json:"agent_secret"`
	Operator      string `json:"operator"`
}

func newConfigShowCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective client configuration (secret masked)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.resolveConfig(cmd, resolveEndpoint(g.dockerContext))
			if err != nil {
				return &cliError{code: usageExitCode, err: err}
			}
			v := buildConfigView(g, cfg)
			if asJSON {
				return printJSON(os.Stdout, v)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintf(w, "config file:\t%s\n", orDash(v.ConfigFile))
			fmt.Fprintf(w, "docker context:\t%s\n", v.DockerContext)
			fmt.Fprintf(w, "manager host:\t%s\n", hostOrDefault(v.ManagerHost))
			fmt.Fprintf(w, "agent port:\t%d\n", v.AgentPort)
			fmt.Fprintf(w, "addr mode:\t%s\n", v.AddrMode)
			fmt.Fprintf(w, "auth mode:\t%s\n", v.AuthMode)
			if v.CA != "" {
				fmt.Fprintf(w, "ca / cert / key:\t%s / %s / %s\n", v.CA, orDash(v.Cert), orDash(v.Key))
			}
			if v.ServerName != "" {
				fmt.Fprintf(w, "server name:\t%s\n", v.ServerName)
			}
			fmt.Fprintf(w, "insecure:\t%v\n", v.Insecure)
			fmt.Fprintf(w, "agent secret:\t%s\n", v.AgentSecret)
			fmt.Fprintf(w, "operator:\t%s\n", orDash(v.Operator))
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON instead of a table")
	return cmd
}

func buildConfigView(g *globalFlags, cfg config.Config) configView {
	cfgFile := g.configPath
	if cfgFile == "" {
		cfgFile = config.DefaultFilePath()
	}
	ctxName := firstNonEmpty(g.dockerContext, os.Getenv("DOCKER_CONTEXT"))
	if ctxName == "" {
		ctxName = dockerctx.Current()
	}
	host, _ := dockerctx.ResolveHost(g.dockerContext)

	secret := "(none)"
	switch {
	case cfg.AgentSecretFile != "":
		secret = "file: " + cfg.AgentSecretFile
	case cfg.AgentSecret != "":
		secret = "set (hidden)"
	}

	authMode := "none / incomplete"
	switch {
	case cfg.CA != "" && cfg.Cert != "" && cfg.Key != "":
		authMode = "mTLS"
	case cfg.AgentSecret != "" || cfg.AgentSecretFile != "":
		authMode = "self-signed + shared secret"
	}

	return configView{
		ConfigFile: cfgFile, DockerContext: ctxName, ManagerHost: host,
		AgentPort: cfg.Port, AddrMode: cfg.AddrMode, AuthMode: authMode,
		CA: cfg.CA, Cert: cfg.Cert, Key: cfg.Key, ServerName: cfg.ServerName,
		Insecure: cfg.Insecure, AgentSecret: secret, Operator: cfg.Operator,
	}
}
