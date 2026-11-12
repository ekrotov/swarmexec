// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/mcpaccess"
	"swarmexec/client/internal/resolve"
)

// `swarmexec mcp` lets an AI assistant look at a swarm through the same two
// channels everything else here uses — the manager API and the node agents —
// over the Model Context Protocol on stdin/stdout.
//
// What it may look at is decided per service by service labels (see
// mcpaccess). Three rules shape every tool in this file, and each one is
// cheaper to keep than to retrofit:
//
//   - The access labels are invisible. They are stripped from every structure
//     before it is rendered, and every string the assistant receives — results
//     and errors — passes mcpaccess.Scrub on its way out (mcpServer.result).
//   - There is no oracle. A service the assistant may not see is "no such
//     service", byte for byte what a service that does not exist gets, and it
//     is absent from every list and count. A visible service it may not read
//     logs of, or exec into, gets a plain "not permitted".
//   - Nothing writes. A future write tool must pass mcpaccess.GuardSpecChange.

type mcpFlags struct {
	defaultAccess  string
	allow          []string
	contexts       []string
	connectTimeout time.Duration
}

func newMCPCmd(g *globalFlags) *cobra.Command {
	f := &mcpFlags{}
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the swarm to an AI assistant over MCP (stdio)",
		Long: "Run a Model Context Protocol server on stdin/stdout, so an AI assistant can\n" +
			"inspect the swarm: services, tasks, logs, stats, stack diffs. Register it with\n\n" +
			"  claude mcp add swarmexec -- swarmexec mcp\n\n" +
			"A service is visible only with the service label swarmexec.mcp.enable=true\n" +
			"(compose: deploy.labels). swarmexec.mcp.logs=true allows its logs,\n" +
			"swarmexec.mcp.exec=true a non-interactive command in its containers. An\n" +
			"explicit \"false\" always wins. --default read makes unlabelled services\n" +
			"visible, never readable or executable.\n\n" +
			"The labels themselves are never shown to the assistant, and nothing can be\n" +
			"changed through this server. A service that mounts the Docker socket is\n" +
			"never exec'd into, whatever its labels say.\n\n" +
			"ssh cannot prompt here: stdin and stdout are the protocol. Load the key for\n" +
			"an ssh:// context into ssh-agent first. Logging goes to the log file.\n\n" +
			"Only the docker context the server starts with (--context, $DOCKER_CONTEXT,\n" +
			"$DOCKER_HOST or docker's current one) is reachable, plus those named with\n" +
			"--contexts. Every other context does not exist as far as the assistant can\n" +
			"tell: it is not listed, and asking for it is \"unknown docker context\".\n\n" +
			"Masking secrets in env and inspect output is cosmetic once exec is granted:\n" +
			"a command run in the container can print its environment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd, g, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.defaultAccess, "default", "none", "access for services without swarmexec.mcp.enable: none | read")
	fl.StringSliceVar(&f.allow, "allow", nil, "enable optional tools: report (security_report)")
	fl.StringSliceVar(&f.contexts, "contexts", nil, "further docker contexts the assistant may use, besides the one the server starts with (comma list or repeated)")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent")
	return cmd
}

func runMCP(cmd *cobra.Command, g *globalFlags, f *mcpFlags) error {
	def, err := mcpaccess.ParseDefault(f.defaultAccess)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	allowReport, err := parseMCPAllow(f.allow)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	extra, err := parseMCPContexts(f.contexts)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	// From here on stdout carries the protocol and nothing else. A stray line
	// on it — a library's fmt.Println, a helper that prints a notice — is a
	// corrupt frame and a dead session, so the real stdout goes to the
	// transport alone and every other writer that asks for os.Stdout gets
	// stderr, which the assistant's host logs.
	protocol := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = protocol }()

	// ssh must never ask. There is no terminal to ask on: stdin is the
	// protocol too, so a passphrase prompt would read a JSON frame as the
	// answer — or hang the session waiting for one.
	sshNonInteractive.Store(true)

	s := newMCPServer(g, cmd, mcpaccess.Policy{Default: def}, allowReport, f.connectTimeout)
	s.allowContexts(extra)
	defer s.close()
	clientlog.L().Info("mcp server starting", "default", def.String(), "report", allowReport,
		"context", s.served, "contexts", strings.Join(extra, ","))

	err = s.build().Run(cmdContext(cmd), &mcp.IOTransport{Reader: os.Stdin, Writer: nopWriteCloser{protocol}})
	clientlog.L().Info("mcp server stopped", "err", errString(err))
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return &cliError{code: 1, err: err}
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// parseMCPAllow reads --allow. Unknown names are an error rather than ignored:
// an operator who typed --allow reprot expects a report tool and should hear
// that there is none.
func parseMCPAllow(vals []string) (report bool, err error) {
	for _, v := range vals {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "report":
			report = true
		case "":
		default:
			return false, fmt.Errorf("--allow: unknown tool %q (known: report)", v)
		}
	}
	return report, nil
}

// parseMCPContexts reads --contexts. A name that is not a docker context is an
// error here, at startup, where the operator sees it — not a puzzle for the
// assistant later, which would only ever hear "unknown docker context".
func parseMCPContexts(vals []string) ([]string, error) {
	var out []string
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" || slices.Contains(out, v) {
			continue
		}
		if v != "default" && !dockerctx.Exists(v) {
			return nil, fmt.Errorf("--contexts: no docker context %q (see swarmexec context ls)", v)
		}
		out = append(out, v)
	}
	return out, nil
}

// mcpServer is the state behind one MCP session.
type mcpServer struct {
	g              *globalFlags
	cmd            *cobra.Command // resolveConfig asks it which flags were set
	policy         mcpaccess.Policy
	allowReport    bool
	connectTimeout time.Duration

	// statsWait bounds how long the stats tool waits for a node's sampler to
	// warm up, asking every statsPoll. Fields so a test need not wait 12s.
	statsWait time.Duration
	statsPoll time.Duration

	// served is the docker context the server was started with, and contexts
	// the ones the assistant may name: served plus --contexts. Any other
	// context is not there for it — not listed, and "unknown" when asked for,
	// byte for byte what a context that does not exist gets. A workstation's
	// context list is a map of every cluster its operator can reach,
	// production included; the operator decides which of them the assistant
	// gets, not the assistant.
	served   string
	contexts map[string]bool

	// clusters are the connections, one per docker context, kept for the
	// session — the same reuse the ui's clusterState gives a cluster switch.
	// An assistant asks many small questions; an ssh handshake through a
	// bastion for each would make every one of them slow.
	mu       sync.Mutex
	clusters map[string]*mcpCluster

	warnMu sync.Mutex
	warned map[string]bool

	// connect builds a cluster for a context name. A field so a test can hand
	// in a fake cluster instead of a real daemon.
	connect func(ctx context.Context, name string) (*mcpCluster, error)
}

// mcpCluster is one docker context's connections.
type mcpCluster struct {
	label string // what reports call this cluster
	dcli  *client.Client
	r     *resolve.Resolver
	cfg   config.Config
	// cfgErr is an agent configuration that does not validate. The manager
	// tools still work without the agents, so it fails only the tools that need
	// them, and says why.
	cfgErr error
	gate   *statsGate
}

func newMCPServer(g *globalFlags, cmd *cobra.Command, p mcpaccess.Policy, allowReport bool, connectTimeout time.Duration) *mcpServer {
	s := &mcpServer{
		g: g, cmd: cmd, policy: p, allowReport: allowReport, connectTimeout: connectTimeout,
		clusters:  map[string]*mcpCluster{},
		warned:    map[string]bool{},
		statsWait: mcpStatsWait,
		statsPoll: mcpStatsPoll,
	}
	s.connect = s.connectContext
	s.allowContexts(nil)
	return s
}

// allowContexts sets the contexts the assistant may use: the one the server
// runs against, plus extra.
func (s *mcpServer) allowContexts(extra []string) {
	s.served = servedContextName(s.g.dockerContext)
	s.contexts = map[string]bool{s.served: true}
	for _, n := range extra {
		s.contexts[n] = true
	}
}

// errUnknownContext is what a context the assistant may not use and one that
// does not exist both get. One function, so the two cannot drift apart.
func errUnknownContext(name string) error {
	return fmt.Errorf("unknown docker context %q (see contexts_list)", name)
}

func (s *mcpServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clusters {
		if c.dcli != nil {
			c.dcli.Close()
		}
	}
	s.clusters = map[string]*mcpCluster{}
}

// mcpOperator is the identity reported to the agents' audit log: the
// operator's own, marked, so an audit trail tells the assistant's actions from
// the operator's. With mTLS the agent takes the identity from the client
// certificate's CN instead; telling the two apart there takes a certificate of
// its own for the assistant.
func mcpOperator(op string) string {
	if op == "" {
		op = "unknown"
	}
	if strings.HasSuffix(op, "+mcp") {
		return op
	}
	return op + "+mcp"
}

// cluster returns the connections for a context, connecting on first use. ""
// is the context the server was started with.
func (s *mcpServer) cluster(ctx context.Context, name string) (*mcpCluster, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == s.served:
		// The served context by name is the served context: one connection, and
		// the same one $DOCKER_HOST picked when that is what "default" means.
		name = ""
	case name != "" && !s.contexts[name]:
		return nil, errUnknownContext(name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clusters[name]; ok {
		return c, nil
	}
	c, err := s.connect(ctx, name)
	if err != nil {
		// Not cached: the next call tries again, which is what an assistant
		// that was just told to load its ssh key will do.
		return nil, err
	}
	s.clusters[name] = c
	return c, nil
}

// connectContext reaches a docker context's manager and prepares the agent
// configuration — one endpoint resolution for both, as everywhere else.
func (s *mcpServer) connectContext(ctx context.Context, name string) (*mcpCluster, error) {
	ctxName := name
	if ctxName == "" {
		ctxName = s.g.dockerContext
	} else if name != "default" && !dockerctx.Exists(name) {
		return nil, errUnknownContext(name)
	}
	ep := resolveEndpoint(ctxName)
	cfg, err := s.g.resolveConfig(s.cmd, ep)
	if err != nil {
		return nil, err
	}
	cfg.Operator = mcpOperator(cfg.Operator)

	cctx, cancel := context.WithTimeout(ctx, clusterProbeTimeout)
	defer cancel()
	dcli, err := ep.connect(cctx)
	if err != nil {
		return nil, err
	}
	// connect forgives a failed ping on purpose (see checkAPIVersion); here an
	// unreachable manager has to be an error now, not an empty answer later.
	if _, err := dcli.Ping(cctx, client.PingOptions{}); err != nil {
		dcli.Close()
		return nil, err
	}
	label := ep.Context
	if label == "" {
		label = servedContextName("")
	}
	return &mcpCluster{
		label:  label,
		dcli:   dcli,
		r:      resolve.New(dcli, addrModeOf(cfg)),
		cfg:    cfg,
		cfgErr: cfg.Validate(),
		gate:   newStatsGate(),
	}, nil
}

// servedContextName names the context a run with this --context value
// actually talks to — "default" when $DOCKER_HOST is what it dials. An
// assistant told which context it is on must hear the one the requests really
// go to. Same resolver as the ui's footer (dockerctx.ResolveName).
func servedContextName(override string) string {
	return dockerctx.ResolveName(override)
}

// agents returns the agent configuration, or why the agents cannot be used.
func (c *mcpCluster) agents() (config.Config, error) {
	if c.cfgErr != nil {
		return c.cfg, fmt.Errorf("agent configuration: %w", c.cfgErr)
	}
	return c.cfg, nil
}

// --- access ------------------------------------------------------------------

// errNotPermitted is the whole answer for a visible service the assistant may
// not read logs of or exec into. It names no label: which label would open
// the door is exactly what it must not learn from asking.
var errNotPermitted = errors.New("not permitted")

// errNoSuchService is what a hidden service and a missing one both get. One
// function, so the two cannot drift apart into an oracle.
func errNoSuchService(name string) error {
	return fmt.Errorf("no such service %q", name)
}

// mcpService is a visible service and what the assistant may do with it.
type mcpService struct {
	svc    swarm.Service
	access mcpaccess.Access
}

// mcpView is the cluster as the assistant may see it: the visible services
// only. Every tool starts from one, so nothing can list a service the policy
// hides by forgetting to filter.
type mcpView struct {
	services []mcpService
	byName   map[string]mcpService
}

// view lists the services and keeps the visible ones.
func (s *mcpServer) view(ctx context.Context, c *mcpCluster) (*mcpView, error) {
	res, err := c.dcli.ServiceList(ctx, client.ServiceListOptions{Status: true})
	if err != nil {
		return nil, err
	}
	v := &mcpView{byName: map[string]mcpService{}}
	for _, svc := range res.Items {
		a := s.evaluate(svc)
		if !a.Visible {
			continue
		}
		ms := mcpService{svc: svc, access: a}
		v.services = append(v.services, ms)
		v.byName[svc.Spec.Name] = ms
	}
	return v, nil
}

// evaluate applies the policy to one service and reports, once per service and
// problem, what it could not make sense of. Only to the log: the assistant is
// not told that an access label exists, let alone that one is misspelled.
func (s *mcpServer) evaluate(svc swarm.Service) mcpaccess.Access {
	a, warns := s.policy.Evaluate(svc.Spec.Labels)
	if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		for k := range cs.Labels {
			if mcpaccess.Reserved(k) {
				warns = append(warns, k+": set as a container label, which grants nothing; use a service label (compose deploy.labels)")
			}
		}
	}
	for _, w := range warns {
		key := svc.Spec.Name + "\x00" + w
		s.warnMu.Lock()
		seen := s.warned[key]
		s.warned[key] = true
		s.warnMu.Unlock()
		if !seen {
			clientlog.L().Warn("mcp access label ignored", "service", svc.Spec.Name, "problem", w)
		}
	}
	return a
}

func (v *mcpView) lookup(name string) (mcpService, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcpService{}, errors.New("a service name is required")
	}
	ms, ok := v.byName[name]
	if !ok {
		return mcpService{}, errNoSuchService(name)
	}
	return ms, nil
}

func (v *mcpView) has(name string) bool {
	_, ok := v.byName[name]
	return ok
}

// visibleOnly keeps the names in list that are visible services.
func (v *mcpView) visibleOnly(list []string) []string {
	var out []string
	for _, n := range list {
		if v.has(n) {
			out = append(out, n)
		}
	}
	return out
}

// --- results -----------------------------------------------------------------

// mcpMaxOutput caps every tool result. An assistant's context is finite and a
// cluster's output is not; a result that fills the window helps nobody, and
// saying it was cut lets the assistant narrow the question instead.
const mcpMaxOutput = 64 << 10

// capText cuts s to max bytes on a rune boundary and says so.
func capText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func truncationNote(max int) string {
	return fmt.Sprintf("\n[output truncated at %d bytes — narrow the request]\n", max)
}

// result turns a tool's outcome into what the assistant receives. Every tool
// goes through here, which is what makes Scrub central rather than a call each
// tool has to remember.
func (s *mcpServer) result(tool string, start time.Time, out string, err error) *mcp.CallToolResult {
	if err != nil {
		msg := mcpaccess.Scrub(s.explain(err))
		if strings.TrimSpace(msg) == "" {
			msg = "the request failed"
		}
		clientlog.L().Info("mcp tool", "tool", tool, "dur", time.Since(start).Round(time.Millisecond).String(), "err", err.Error())
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
	}
	text := mcpaccess.Scrub(out)
	if t, cut := capText(text, mcpMaxOutput); cut {
		text = t + truncationNote(mcpMaxOutput)
	}
	clientlog.L().Info("mcp tool", "tool", tool, "dur", time.Since(start).Round(time.Millisecond).String(), "bytes", len(text))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// explain makes an error actionable for whoever reads the assistant's answer.
// The one that needs it most is ssh: this server cannot ask for a passphrase,
// so a key that is not in the agent fails, and the fix is outside the session.
func (s *mcpServer) explain(err error) string {
	if isSSHAuthError(err) {
		return "ssh could not sign in to the cluster. This server cannot ask for a passphrase " +
			"(stdin and stdout carry the protocol), so the key must be loaded in ssh-agent: run " +
			"`ssh-add` in a shell whose SSH_AUTH_SOCK the assistant's host inherits, then retry.\n\n" + err.Error()
	}
	if strings.Contains(err.Error(), "Host key verification failed") {
		// The other question ssh would have asked: whether to trust the host.
		return "ssh does not know this host's key, and this server cannot ask whether to trust it. " +
			"Connect once with plain `ssh` (or `swarmexec ps`) to record the key in known_hosts, then retry.\n\n" + err.Error()
	}
	return err.Error()
}

// jsonText renders a tool's structured answer. JSON rather than a table: the
// reader is a model, and field names it does not have to guess are worth more
// to it than alignment.
func jsonText(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	// "<masked>" and "a -> b" are for a reader, not a browser.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// capWriter keeps at most max bytes and remembers whether more arrived. onFull
// runs once, the first time something is dropped — a log read uses it to stop
// streaming rather than read a log to its end only to throw it away.
type capWriter struct {
	mu        sync.Mutex
	buf       strings.Builder
	max       int
	truncated bool
	onFull    func()
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.max - w.buf.Len()
	if room >= len(p) {
		w.buf.Write(p)
		return len(p), nil
	}
	if room > 0 {
		w.buf.Write(p[:room])
	}
	if !w.truncated {
		w.truncated = true
		if w.onFull != nil {
			go w.onFull()
		}
	}
	return len(p), nil // swallowed, not failed: the stream must not error out
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, _ := capText(w.buf.String(), w.max)
	return s
}

func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
