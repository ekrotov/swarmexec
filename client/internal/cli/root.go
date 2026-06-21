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
	configPath string
	ca         string
	cert       string
	key        string
	port       int
	addrMode   string
	serverName string
}

// Execute builds and runs the root command, returning a process exit code.
func Execute(v Version) int {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "swarmexec",
		Short:         "Cluster-wide docker exec for Docker Swarm",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       fmt.Sprintf("%s (proto %s)", v.Binary, v.Proto),
	}
	root.SetVersionTemplate("swarmexec {{.Version}}\n")

	pf := root.PersistentFlags()
	pf.StringVar(&g.configPath, "config", "", "config file path (default ~/.config/swarmexec/config.yaml)")
	pf.StringVar(&g.ca, "ca", "", "CA certificate to verify the agent (mTLS)")
	pf.StringVar(&g.cert, "cert", "", "client certificate (mTLS; CN is the operator identity)")
	pf.StringVar(&g.key, "key", "", "client private key (mTLS)")
	pf.IntVar(&g.port, "port", 0, fmt.Sprintf("agent port (default %d)", config.DefaultPort))
	pf.StringVar(&g.addrMode, "addr-mode", "", "node dial address: hostname|ip (default hostname)")
	pf.StringVar(&g.serverName, "server-name", "", "override TLS server name for agent verification")

	root.AddCommand(newExecCmd(g))
	root.AddCommand(newPsCmd(g))

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
	return cfg, nil
}

func addrModeOf(cfg config.Config) resolve.AddrMode {
	if cfg.AddrMode == config.AddrModeIP {
		return resolve.AddrIP
	}
	return resolve.AddrHostname
}
