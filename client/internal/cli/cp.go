// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/cptar"
	"swarmexec/client/internal/endpoint"
	"swarmexec/client/internal/resolve"
	cterm "swarmexec/client/internal/term"
	"swarmexec/internal/dockerfmt"
)

type cpFlags struct {
	archive        bool
	followLink     bool
	maxSize        string
	quiet          bool
	node           string
	connectTimeout time.Duration
}

func newCpCmd(g *globalFlags) *cobra.Command {
	f := &cpFlags{}
	cmd := &cobra.Command{
		Use:   "cp [flags] <src> <dst>",
		Short: "Copy files and directories to, from and between containers in the swarm",
		Long: `Move files in or out of a container on whatever node of the swarm it runs —
and between two containers, which may sit on different nodes.

Either side is a local path or <target>:<path>, where the target is what exec
takes: a service, a service.slot, a task id or a container id. One side must
name a container; two local paths are a job for cp(1).

Nothing has to exist inside the container — not even tar. The copy goes
through the Engine's own archive endpoint on the node, so it works with
distroless and scratch images, which is where it is needed most.

The destination rules are docker cp's: a path that is an existing directory
receives the source under its own name, any other path IS the name. A copy
between two containers never touches this machine's disk, but every byte does
travel through it: there is no node-to-node path, by design.`,
		Example: "  swarmexec cp ./patch.sql db:/tmp/\n" +
			"  swarmexec cp web.1:/var/log/app.log ./app.log\n" +
			"  swarmexec cp web.1:/etc/nginx.conf db.2:/tmp/",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCp(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&f.archive, "archive", "a", false, "keep the owner and group recorded in the archive")
	fl.BoolVarP(&f.followLink, "follow-link", "L", false, "copy what a symlinked source points at, not the link")
	fl.StringVar(&f.maxSize, "max-size", "1G", "refuse a copy larger than this (K/M/G, binary units)")
	fl.BoolVarP(&f.quiet, "quiet", "q", false, "no progress output")
	fl.StringVar(&f.node, "node", "", "node hint/override for container-id targets")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
	return cmd
}

// cpSpec is one side of the copy: a local path, or a path in a container.
type cpSpec struct {
	target string // empty for a local path
	path   string
}

func (s cpSpec) remote() bool { return s.target != "" }

// parseCpSpec splits "<target>:<path>" from a local path. A prefix that
// contains a separator is a local path (./a:b), and so is a single-letter one
// (C:\tmp): a Windows drive is not a service name.
func parseCpSpec(arg string) cpSpec {
	i := strings.Index(arg, ":")
	if i <= 1 || strings.ContainsAny(arg[:i], `/\`) {
		return cpSpec{path: arg}
	}
	return cpSpec{target: arg[:i], path: arg[i+1:]}
}

func runCp(cmd *cobra.Command, g *globalFlags, f *cpFlags, args []string) error {
	src, dst := parseCpSpec(args[0]), parseCpSpec(args[1])
	switch {
	case !src.remote() && !dst.remote():
		return &cliError{code: exitUsage, err: fmt.Errorf("neither side names a container; this is a job for cp(1)")}
	case src.path == "" || dst.path == "":
		return &cliError{code: exitUsage, err: fmt.Errorf("both sides need a path")}
	case src.path == "-" || dst.path == "-":
		return &cliError{code: exitUsage, err: fmt.Errorf(`"-" (stdin/stdout) is not supported; name a file`)}
	}
	maxBytes, err := units.RAMInBytes(f.maxSize)
	if err != nil || maxBytes <= 0 {
		return &cliError{code: exitUsage, err: fmt.Errorf("--max-size %q: not a size", f.maxSize)}
	}

	dockerEP := endpoint.Resolve(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	ctx := cmdContext(cmd)
	dcli, err := dockerEP.Connect(ctx)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ac := agentcall.New(cfg)
	defer ac.Close()

	c := &cpRun{ac: ac, r: r, f: f, maxBytes: uint64(maxBytes), out: cmd.ErrOrStderr(), stdinIsTerm: cterm.IsTerminal(os.Stdin.Fd())}
	switch {
	case src.remote() && dst.remote():
		err = c.betweenContainers(ctx, src, dst)
	case src.remote():
		err = c.fromContainer(ctx, src, dst.path)
	default:
		err = c.toContainer(ctx, src.path, dst)
	}
	if err != nil {
		return err
	}
	return nil
}

// cpRun is one `cp` invocation: the pieces every direction needs.
type cpRun struct {
	ac          *agentcall.Client
	r           *resolve.Resolver
	f           *cpFlags
	maxBytes    uint64
	out         io.Writer
	stdinIsTerm bool
}

// endpointOf resolves one container target, asking which replica when a
// service has several and a terminal is there to ask.
func (c *cpRun) endpointOf(ctx context.Context, target string) (resolve.Endpoint, error) {
	ep, err := c.r.Resolve(ctx, resolve.Request{Target: target, NodeHint: c.f.node})
	if err != nil {
		amb, ok := err.(*resolve.AmbiguousError)
		if !ok {
			return resolve.Endpoint{}, &cliError{code: exitTransport, err: err}
		}
		ep, err = pickCandidate(amb, c.stdinIsTerm, c.out)
		if err != nil {
			return resolve.Endpoint{}, &cliError{code: exitTransport, err: err}
		}
	}
	return *ep, nil
}

// fromContainer copies a path from a container onto this machine.
func (c *cpRun) fromContainer(ctx context.Context, src cpSpec, localDst string) error {
	ep, err := c.endpointOf(ctx, src.target)
	if err != nil {
		return err
	}
	stat, exists, err := c.ac.StatPath(ctx, ep, c.f.connectTimeout, src.path)
	if err != nil {
		return copyError(err)
	}
	if !exists {
		return &cliError{code: exitNo, err: fmt.Errorf("%s:%s does not exist", src.target, src.path)}
	}
	dir, opts, err := localDestination(localDst, stat.GetIsDir() || contentsOf(src.path))
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	opts.MaxBytes = int64(c.maxBytes)

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, _, cerr := c.ac.CopyFrom(ctx, ep, c.f.connectTimeout, agentcall.CopyFromOptions{
			Path: src.path, FollowLink: c.f.followLink, MaxBytes: c.maxBytes,
		}, pw, c.progress())
		_ = pw.CloseWithError(cerr)
		done <- cerr
	}()
	rep, xerr := cptar.Extract(pr, dir, opts)
	_ = pr.CloseWithError(xerr)
	if cerr := <-done; cerr != nil {
		return copyError(cerr)
	}
	if xerr != nil {
		return &cliError{code: exitNo, err: xerr}
	}
	c.report(rep, rep.Bytes)
	return nil
}

// toContainer copies a local path into a container.
func (c *cpRun) toContainer(ctx context.Context, localSrc string, dst cpSpec) error {
	fi, err := os.Stat(localSrc)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	ep, err := c.endpointOf(ctx, dst.target)
	if err != nil {
		return err
	}
	destDir, name, err := c.remoteDestination(ctx, ep, dst.path, filepath.Base(localSrc))
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	packed := make(chan cptar.Report, 1)
	go func() {
		rep, perr := cptar.Pack(pw, localSrc, name)
		_ = pw.CloseWithError(perr)
		packed <- rep
	}()
	_, err = c.ac.CopyTo(ctx, ep, c.f.connectTimeout, agentcall.CopyToOptions{
		DestDir: destDir, CopyUIDGID: c.f.archive, MaxBytes: c.maxBytes,
	}, pr, c.progress())
	_ = pr.Close()
	rep := <-packed
	if err != nil {
		return copyError(err)
	}
	// The file's own bytes, not the archive's: a 10-byte file in a tar is
	// 2 KiB of padding, and reporting that would read as a wrong number.
	c.done(fi.Name(), rep.Bytes)
	return nil
}

// betweenContainers copies from one container to another. The archive is rewritten in
// flight — the source's root entry renamed to what the destination should be
// called — so nothing is ever written to this machine's disk.
func (c *cpRun) betweenContainers(ctx context.Context, src, dst cpSpec) error {
	srcEP, err := c.endpointOf(ctx, src.target)
	if err != nil {
		return err
	}
	dstEP, err := c.endpointOf(ctx, dst.target)
	if err != nil {
		return err
	}
	stat, exists, err := c.ac.StatPath(ctx, srcEP, c.f.connectTimeout, src.path)
	if err != nil {
		return copyError(err)
	}
	if !exists {
		return &cliError{code: exitNo, err: fmt.Errorf("%s:%s does not exist", src.target, src.path)}
	}
	destDir, name, err := c.remoteDestination(ctx, dstEP, dst.path, path.Base(path.Clean(src.path)))
	if err != nil {
		return err
	}

	// Two pipes with the rename between them: agent A -> rename -> agent B.
	fromR, fromW := io.Pipe()
	toR, toW := io.Pipe()
	fromErr := make(chan error, 1)
	renameErr := make(chan error, 1)
	go func() {
		_, _, cerr := c.ac.CopyFrom(ctx, srcEP, c.f.connectTimeout, agentcall.CopyFromOptions{
			Path: src.path, FollowLink: c.f.followLink, MaxBytes: c.maxBytes,
		}, fromW, nil)
		_ = fromW.CloseWithError(cerr)
		fromErr <- cerr
	}()
	go func() {
		rerr := cptar.Rename(fromR, toW, cptar.ExtractOptions{Rename: name})
		_ = toW.CloseWithError(rerr)
		renameErr <- rerr
	}()
	_, err = c.ac.CopyTo(ctx, dstEP, c.f.connectTimeout, agentcall.CopyToOptions{
		DestDir: destDir, CopyUIDGID: c.f.archive, MaxBytes: c.maxBytes,
	}, toR, c.progress())
	_ = toR.Close()
	_ = fromR.Close()
	if cerr := <-fromErr; cerr != nil {
		return copyError(cerr)
	}
	if rerr := <-renameErr; rerr != nil {
		return &cliError{code: exitNo, err: rerr}
	}
	if err != nil {
		return copyError(err)
	}
	c.done(stat.GetName(), stat.GetSize())
	return nil
}

// remoteDestination works out the directory the agent extracts into and the
// name the source takes there, the way `docker cp` does: an existing directory
// receives the source under its own name, anything else IS the name.
//
// It asks the agent to stat the destination. A policy that allows copy.to but
// not copy.from refuses that question; the trailing slash then decides, which
// is the rule the help text states.
func (c *cpRun) remoteDestination(ctx context.Context, ep resolve.Endpoint, dest, srcName string) (dir, name string, err error) {
	clean := path.Clean(dest)
	stat, exists, serr := c.ac.StatPath(ctx, ep, c.f.connectTimeout, clean)
	switch {
	case serr != nil && status.Code(serr) != codes.PermissionDenied:
		return "", "", copyError(serr)
	case serr != nil, !exists:
		// Unknown: the trailing slash is the operator's own statement.
		if strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, "/.") {
			return clean, srcName, nil
		}
		return path.Dir(clean), path.Base(clean), nil
	case stat.GetIsDir():
		return clean, srcName, nil
	default:
		return path.Dir(clean), path.Base(clean), nil
	}
}

// localDestination works out which directory to extract into and how to rename
// the archive's root for a copy onto this machine. srcIsDir decides between
// "this path becomes the directory" and "this path becomes the file".
func localDestination(dest string, srcIsDir bool) (dir string, opts cptar.ExtractOptions, err error) {
	fi, statErr := os.Stat(dest)
	switch {
	case statErr == nil && fi.IsDir():
		return dest, cptar.ExtractOptions{}, nil // src lands under its own name
	case statErr == nil && srcIsDir:
		return "", opts, fmt.Errorf("%s is a file; a directory cannot be copied onto it", dest)
	case srcIsDir:
		// A new directory, holding what the source holds.
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return "", opts, err
		}
		return dest, cptar.ExtractOptions{StripRoot: true}, nil
	default:
		parent := filepath.Dir(dest)
		if _, err := os.Stat(parent); err != nil {
			return "", opts, err
		}
		return parent, cptar.ExtractOptions{Rename: filepath.Base(dest)}, nil
	}
}

// copyError turns an agent's answer into an exit code. A refusal the operator
// caused — denied by policy, over the size cap, no such path, a destination
// the Engine will not overwrite — is a "no" (1), not a transport failure
// (125): it says something true about the request, and a script should be
// able to tell the two apart.
func copyError(err error) error {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.ResourceExhausted, codes.NotFound,
		codes.FailedPrecondition, codes.InvalidArgument:
		return &cliError{code: exitNo, err: err}
	default:
		return &cliError{code: exitTransport, err: err}
	}
}

// contentsOf reports whether the source path asks for a directory's contents
// ("dir/." in docker cp), which the Engine answers with the directory itself.
func contentsOf(p string) bool { return strings.HasSuffix(p, "/.") }

// progress returns the callback that keeps a byte count on the screen, or nil
// when nobody is watching.
func (c *cpRun) progress() func(int64) {
	if c.f.quiet || !cterm.IsTerminal(os.Stderr.Fd()) {
		return nil
	}
	last := time.Now()
	return func(n int64) {
		if time.Since(last) < 100*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Fprintf(c.out, "\r%s copied", dockerfmt.Bytes(n))
	}
}

// done prints the one summary line a successful copy leaves behind.
func (c *cpRun) done(name string, n int64) {
	if c.f.quiet {
		return
	}
	fmt.Fprintf(c.out, "\r%s: %s copied\n", name, dockerfmt.Bytes(n))
}

// report prints what an extraction onto this machine did, including anything
// it refused — a file missing from a copy must never be silent.
func (c *cpRun) report(rep cptar.Report, n int64) {
	for _, r := range rep.Refused {
		fmt.Fprintf(c.out, "refused: %s\n", r)
	}
	if c.f.quiet {
		return
	}
	fmt.Fprintf(c.out, "\r%s copied in %d file(s)\n", dockerfmt.Bytes(n), rep.Files)
}
