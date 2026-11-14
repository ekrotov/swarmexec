// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package mcp is the server behind `swarmexec mcp`: it lets an AI assistant
// look at a swarm through the same two channels everything else here uses —
// the manager API and the node agents — over the Model Context Protocol.
//
// What it may look at is decided per service by service labels (see
// mcpaccess). Three rules shape every tool in this package, and each one is
// cheaper to keep than to retrofit:
//
//   - The access labels are invisible. They are stripped from every structure
//     before it is rendered, and every string the assistant receives — results
//     and errors — passes mcpaccess.Scrub on its way out (Server.result).
//   - There is no oracle. A service the assistant may not see is "no such
//     service", byte for byte what a service that does not exist gets, and it
//     is absent from every list and count. A visible service it may not read
//     logs of, or exec into, gets a plain "not permitted".
//   - Nothing writes. A future write tool must pass mcpaccess.GuardSpecChange.
//
// The package reads the cluster through swarmops, logstream, agentcall and
// stackfile and never imports the cli (boundary_test.go). Reaching a docker
// context — endpoint resolution, ssh, the agent configuration — is the
// caller's: the cli's `mcp` command hands it in as Options.Dial. The MCP SDK
// is imported as sdk, since this package is called mcp too.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/mcpaccess"
	"swarmexec/client/internal/resolve"
)

// ParseAllow reads --allow. Unknown names are an error rather than ignored:
// an operator who typed --allow reprot expects a report tool and should hear
// that there is none.
func ParseAllow(vals []string) (report bool, err error) {
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

// ParseContexts reads --contexts. A name that is not a docker context is an
// error here, at startup, where the operator sees it — not a puzzle for the
// assistant later, which would only ever hear "unknown docker context".
func ParseContexts(vals []string) ([]string, error) {
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

// Options configures a server: what the assistant may see and do, which
// docker contexts it may use, and how to reach one. Reaching a context —
// endpoint resolution, ssh, the agent configuration from flags, environment
// and file — is the caller's: it hands in Dial.
type Options struct {
	// Policy decides per service what the assistant may see, read and exec.
	Policy mcpaccess.Policy
	// AllowReport enables the security_report tool (--allow report).
	AllowReport bool
	// ConnectTimeout bounds each agent connection.
	ConnectTimeout time.Duration
	// DockerContext is the context the server was started with (--context;
	// "" leaves the choice to docker: $DOCKER_HOST, $DOCKER_CONTEXT, current).
	DockerContext string
	// Contexts are the further contexts the assistant may use (--contexts).
	Contexts []string
	// ClientVersion and ClientProto are this client's, for the doctor tool,
	// the report and the MCP handshake.
	ClientVersion, ClientProto string
	// Dial reaches a docker context's manager and resolves the agent
	// configuration that goes with it. The name is never "": the server
	// passes DockerContext for its own context.
	Dial func(ctx context.Context, contextName string) (Endpoint, error)
	// IsSSHAuthError reports whether err is ssh failing to sign in, which
	// gets an explanation of its own (nil: never).
	IsSSHAuthError func(error) bool
}

// Endpoint is a docker context reached through Options.Dial.
type Endpoint struct {
	// Label is the context's name as the requests really go to it; "" for
	// docker's own choice, which the server names itself.
	Label string
	// Docker is the manager connection; the server owns it from here on.
	Docker *client.Client
	// Config is the agent configuration for this context.
	Config config.Config
}

// Server is the state behind one MCP session.
type Server struct {
	opts           Options
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
	agent *agentcall.Client
	// cfgErr is an agent configuration that does not validate. The manager
	// tools still work without the agents, so it fails only the tools that need
	// them, and says why.
	cfgErr error
}

func New(o Options) *Server {
	s := &Server{
		opts: o, policy: o.Policy, allowReport: o.AllowReport, connectTimeout: o.ConnectTimeout,
		clusters:  map[string]*mcpCluster{},
		warned:    map[string]bool{},
		statsWait: mcpStatsWait,
		statsPoll: mcpStatsPoll,
	}
	s.connect = s.connectContext
	s.allowContexts(o.Contexts)
	return s
}

// Serve runs the MCP session over in/out until the client goes away or ctx
// ends. A clean end — the client closed the stream, ctx was cancelled — is a
// nil error.
func (s *Server) Serve(ctx context.Context, in io.ReadCloser, out io.Writer) error {
	clientlog.L().Info("mcp server starting", "default", s.policy.Default.String(), "report", s.allowReport,
		"context", s.served, "contexts", strings.Join(s.opts.Contexts, ","))
	err := s.build().Run(ctx, &sdk.IOTransport{Reader: in, Writer: nopWriteCloser{out}})
	clientlog.L().Info("mcp server stopped", "err", errString(err))
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// allowContexts sets the contexts the assistant may use: the one the server
// runs against, plus extra.
func (s *Server) allowContexts(extra []string) {
	s.served = ServedContextName(s.opts.DockerContext)
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

// Close drops the session's connections.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clusters {
		if c.dcli != nil {
			c.dcli.Close()
		}
		if c.agent != nil {
			c.agent.Close()
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
func (s *Server) cluster(ctx context.Context, name string) (*mcpCluster, error) {
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

// mcpProbeTimeout bounds reaching a context's manager: the dial (an ssh
// handshake through a bastion included) and the ping after it. The TUI's
// cluster probe uses the same budget.
const mcpProbeTimeout = 15 * time.Second

// connectContext reaches a docker context's manager and prepares the agent
// configuration — one endpoint resolution for both, as everywhere else.
func (s *Server) connectContext(ctx context.Context, name string) (*mcpCluster, error) {
	ctxName := name
	if ctxName == "" {
		ctxName = s.opts.DockerContext
	} else if name != "default" && !dockerctx.Exists(name) {
		return nil, errUnknownContext(name)
	}

	cctx, cancel := context.WithTimeout(ctx, mcpProbeTimeout)
	defer cancel()
	ep, err := s.opts.Dial(cctx, ctxName)
	if err != nil {
		return nil, err
	}
	dcli, cfg := ep.Docker, ep.Config
	cfg.Operator = mcpOperator(cfg.Operator)
	// Dial may forgive a failed ping (the cli's connect does, on purpose);
	// here an unreachable manager has to be an error now, not an empty answer
	// later.
	if _, err := dcli.Ping(cctx, client.PingOptions{}); err != nil {
		dcli.Close()
		return nil, err
	}
	label := ep.Label
	if label == "" {
		label = ServedContextName("")
	}
	return &mcpCluster{
		label:  label,
		dcli:   dcli,
		r:      resolve.New(dcli, agentcall.AddrMode(cfg)),
		agent:  agentcall.New(cfg),
		cfgErr: cfg.Validate(),
	}, nil
}

// ServedContextName names the context a run with this --context value
// actually talks to — "default" when $DOCKER_HOST is what it dials. An
// assistant told which context it is on must hear the one the requests really
// go to. Same resolver as the ui's footer (dockerctx.ResolveName).
func ServedContextName(override string) string {
	return dockerctx.ResolveName(override)
}

// agents returns the cluster's agent client, or why the agents cannot be used.
func (c *mcpCluster) agents() (*agentcall.Client, error) {
	if c.cfgErr != nil {
		return nil, fmt.Errorf("agent configuration: %w", c.cfgErr)
	}
	return c.agent, nil
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
func (s *Server) view(ctx context.Context, c *mcpCluster) (*mcpView, error) {
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
func (s *Server) evaluate(svc swarm.Service) mcpaccess.Access {
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
func (s *Server) result(tool string, start time.Time, out string, err error) *sdk.CallToolResult {
	if err != nil {
		msg := mcpaccess.Scrub(s.explain(err))
		if strings.TrimSpace(msg) == "" {
			msg = "the request failed"
		}
		clientlog.L().Info("mcp tool", "tool", tool, "dur", time.Since(start).Round(time.Millisecond).String(), "err", err.Error())
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
	}
	text := mcpaccess.Scrub(out)
	if t, cut := capText(text, mcpMaxOutput); cut {
		text = t + truncationNote(mcpMaxOutput)
	}
	clientlog.L().Info("mcp tool", "tool", tool, "dur", time.Since(start).Round(time.Millisecond).String(), "bytes", len(text))
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

// explain makes an error actionable for whoever reads the assistant's answer.
// The one that needs it most is ssh: this server cannot ask for a passphrase,
// so a key that is not in the agent fails, and the fix is outside the session.
func (s *Server) explain(err error) string {
	if s.opts.IsSSHAuthError != nil && s.opts.IsSSHAuthError(err) {
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
