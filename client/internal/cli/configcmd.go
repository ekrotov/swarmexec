// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/endpoint"
)

func newConfigCmd(g *globalFlags) *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "config",
		Short: "Inspect the resolved client configuration",
	})
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
	// Sources says where each credential/dial value in effect came from: a
	// flag, an environment variable, contexts.<name>.<key>, the global file
	// key, or the built-in default.
	Sources map[string]string `json:"sources"`
	// Underlay is the resolved context's contexts.<name>.underlay.
	Underlay []string `json:"underlay,omitempty"`
	// ConfirmSwitch is the resolved context's contexts.<name>.confirm_switch.
	ConfirmSwitch bool `json:"confirm_switch"`
	// SyncDockerContext is ui.sync_docker_context.
	SyncDockerContext bool `json:"sync_docker_context"`
	// CheckUpdates is ui.check_updates after SWARMEXEC_NO_UPDATE_CHECK.
	CheckUpdates bool `json:"check_updates"`
	// CheckRegistry is ui.check_registry after SWARMEXEC_NO_REGISTRY_CHECK.
	CheckRegistry bool `json:"check_registry"`
}

func newConfigShowCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective client configuration (secret masked)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.resolveConfig(cmd, endpoint.Resolve(g.dockerContext))
			if err != nil {
				return &cliError{code: exitUsage, err: err}
			}
			v := buildConfigView(g, cfg)
			if asJSON {
				return printJSON(os.Stdout, v)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintf(w, "config file:\t%s\n", orDash(v.ConfigFile))
			fmt.Fprintf(w, "docker context:\t%s\n", v.DockerContext)
			fmt.Fprintf(w, "manager host:\t%s\n", hostOrDefault(v.ManagerHost))
			from := func(key string) string { return "  (" + v.Sources[key] + ")" }
			fmt.Fprintf(w, "agent port:\t%d%s\n", v.AgentPort, from(config.KeyPort))
			fmt.Fprintf(w, "addr mode:\t%s%s\n", v.AddrMode, from(config.KeyAddrMode))
			fmt.Fprintf(w, "auth mode:\t%s\n", v.AuthMode)
			if v.CA != "" {
				fmt.Fprintf(w, "ca / cert / key:\t%s / %s / %s  (%s / %s / %s)\n", v.CA, orDash(v.Cert), orDash(v.Key),
					v.Sources[config.KeyCA], v.Sources[config.KeyCert], v.Sources[config.KeyKey])
			}
			if v.ServerName != "" {
				fmt.Fprintf(w, "server name:\t%s%s\n", v.ServerName, from(config.KeyServerName))
			}
			fmt.Fprintf(w, "insecure:\t%v%s\n", v.Insecure, from(config.KeyInsecure))
			fmt.Fprintf(w, "agent secret:\t%s%s\n", v.AgentSecret, from(config.KeyAgentSecret))
			fmt.Fprintf(w, "operator:\t%s\n", orDash(v.Operator))
			if len(v.Underlay) > 0 {
				fmt.Fprintf(w, "underlay:\t%s\n", strings.Join(v.Underlay, ", "))
			}
			if v.ConfirmSwitch {
				fmt.Fprintf(w, "confirm switch:\t%v (the TUI asks before switching to %s)\n", v.ConfirmSwitch, v.DockerContext)
			}
			fmt.Fprintf(w, "sync docker context:\t%v\n", v.SyncDockerContext)
			fmt.Fprintf(w, "check updates:\t%v\n", v.CheckUpdates)
			fmt.Fprintf(w, "check registry:\t%v\n", v.CheckRegistry)
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
	// One resolver for the name and the host, so `config show` cannot print the
	// stored current context next to $DOCKER_HOST's endpoint.
	ctxName := dockerctx.ResolveName(g.dockerContext)
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

	sources := map[string]string{}
	for _, k := range []string{config.KeyCA, config.KeyCert, config.KeyKey, config.KeyPort,
		config.KeyAddrMode, config.KeyServerName, config.KeyAgentSecret, config.KeyInsecure} {
		sources[k] = cfg.Source(k)
	}
	return configView{
		Sources:    sources,
		ConfigFile: cfgFile, DockerContext: ctxName, ManagerHost: host,
		AgentPort: cfg.Port, AddrMode: cfg.AddrMode, AuthMode: authMode,
		CA: cfg.CA, Cert: cfg.Cert, Key: cfg.Key, ServerName: cfg.ServerName,
		Insecure: cfg.Insecure, AgentSecret: secret, Operator: cfg.Operator,
		Underlay:          underlayStrings(cfg.Underlay(ctxName)),
		ConfirmSwitch:     cfg.ConfirmSwitch(ctxName),
		SyncDockerContext: cfg.UI.SyncDockerContext,
		CheckUpdates:      cfg.UI.UpdateChecks(),
		CheckRegistry:     cfg.UI.RegistryChecks(),
	}
}

// underlayStrings renders parsed underlay CIDRs for display.
func underlayStrings(ps []netip.Prefix) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}
