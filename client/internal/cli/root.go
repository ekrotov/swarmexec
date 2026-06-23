// Package cli implements the swarmexec command surface (exec, ps, version) on
// top of cobra, wires flags/env/config into a resolved configuration, and maps
// outcomes to process exit codes (REQUIREMENTS §3, §6, §8).
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
)

// Version info, injected from main via -ldflags.
type Version struct {
	Binary string
	Proto  string
}

// globalFlags hold settings shared by all subcommands.
type globalFlags struct {
	configPath      string
	ca              string
	cert            string
	key             string
	port            int
	addrMode        string
	serverName      string
	agentSecret     string
	agentSecretFile string
	insecure        bool
	operator        string
	dockerContext   string
	version         Version // build info, for `doctor` skew checks
}

// licenseName is shown by --info. See the LICENSE file for the full text.
const licensePlaceholder = "Proprietary — © 2026 Cloud Surfers (all rights reserved)"

// contactEmail is the maintainer contact shown by --info.
const contactEmail = "eugen.krotov@cloud-surfers.de"

// infoText renders the --info block.
func infoText(v Version) string {
	return fmt.Sprintf("swarmexec %s\n  protocol: %s\n  license:  %s\n  contact:  %s\n",
		v.Binary, v.Proto, licensePlaceholder, contactEmail)
}

// Execute builds and runs the root command, returning a process exit code.
func Execute(v Version) int {
	g := &globalFlags{version: v}
	var showInfo bool
	root := &cobra.Command{
		Use:           "swarmexec",
		Short:         "Cluster-wide docker exec for Docker Swarm",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       fmt.Sprintf("%s (proto %s)", v.Binary, v.Proto),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showInfo {
				fmt.Fprint(cmd.OutOrStdout(), infoText(v))
				return nil
			}
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("swarmexec {{.Version}}\n")
	root.Flags().BoolVar(&showInfo, "info", false, "show version, license, and contact info")

	pf := root.PersistentFlags()
	pf.StringVar(&g.configPath, "config", "", "config file path (default ~/.config/swarmexec/config.yaml)")
	pf.StringVar(&g.ca, "ca", "", "CA certificate to verify the agent (mTLS)")
	pf.StringVar(&g.cert, "cert", "", "client certificate (mTLS; CN is the operator identity)")
	pf.StringVar(&g.key, "key", "", "client private key (mTLS)")
	pf.IntVar(&g.port, "port", 0, fmt.Sprintf("agent port (default %d)", config.DefaultPort))
	pf.StringVar(&g.addrMode, "addr-mode", "", "node dial address: hostname|ip (default hostname)")
	pf.StringVar(&g.serverName, "server-name", "", "override TLS server name for agent verification")
	pf.StringVar(&g.agentSecret, "agent-secret", "", "shared secret for a self-signed agent (Portainer-style auth)")
	pf.StringVar(&g.agentSecretFile, "agent-secret-file", "", "file to read the shared secret from")
	pf.BoolVar(&g.insecure, "insecure", false, "skip agent server-certificate verification (self-signed agents)")
	pf.StringVar(&g.operator, "operator", "", "operator identity reported for audit (default: OS username)")
	pf.StringVar(&g.dockerContext, "context", "", "docker context for the manager API; supports ssh:// (also $DOCKER_CONTEXT)")

	root.AddCommand(newInitCmd(g))
	root.AddCommand(newDownCmd(g))
	root.AddCommand(newDoctorCmd(g))
	root.AddCommand(newConfigCmd(g))
	root.AddCommand(newExecCmd(g))
	root.AddCommand(newPsCmd(g))
	root.AddCommand(newLogsCmd(g))
	root.AddCommand(newVolumeCmd(g))
	root.AddCommand(newUICmd(g))

	err := root.Execute()
	if err == nil {
		return 0
	}
	var ce *cliError
	if errors.As(err, &ce) {
		if !ce.silent && ce.err != nil {
			fmt.Fprintln(os.Stderr, "swarmexec: "+ce.err.Error())
		}
		return ce.code
	}
	// cobra usage / flag-parse errors.
	fmt.Fprintln(os.Stderr, "swarmexec: "+err.Error())
	return usageExitCode
}

// resolveConfig merges defaults + file + env, then overlays explicitly-set
// global flags.
func (g *globalFlags) resolveConfig(cmd *cobra.Command) (config.Config, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return cfg, err
	}
	pf := cmd.Flags()
	if pf.Changed("ca") {
		cfg.CA = g.ca
	}
	if pf.Changed("cert") {
		cfg.Cert = g.cert
	}
	if pf.Changed("key") {
		cfg.Key = g.key
	}
	if pf.Changed("port") {
		cfg.Port = g.port
	}
	if pf.Changed("addr-mode") {
		cfg.AddrMode = g.addrMode
	}
	if pf.Changed("server-name") {
		cfg.ServerName = g.serverName
	}
	if pf.Changed("agent-secret") {
		cfg.AgentSecret = g.agentSecret
	}
	if pf.Changed("agent-secret-file") {
		cfg.AgentSecretFile = g.agentSecretFile
	}
	if pf.Changed("insecure") {
		cfg.Insecure = g.insecure
	}
	if pf.Changed("operator") {
		cfg.Operator = g.operator
	}
	return cfg, nil
}

func addrModeOf(cfg config.Config) resolve.AddrMode {
	if cfg.AddrMode == config.AddrModeIP {
		return resolve.AddrIP
	}
	return resolve.AddrHostname
}
