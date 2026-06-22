package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
)

type logsFlags struct {
	follow         bool
	tail           uint32
	timestamps     bool
	since          time.Duration
	node           string
	connectTimeout time.Duration
}

func newLogsCmd(g *globalFlags) *cobra.Command {
	f := &logsFlags{}
	cmd := &cobra.Command{
		Use:   "logs [flags] <service|service.slot|task-id|container-id>",
		Short: "Stream a container's logs from anywhere in the swarm",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&f.follow, "follow", "f", false, "keep streaming new log lines")
	fl.Uint32Var(&f.tail, "tail", 0, "number of lines from the end to start with (0 = all)")
	fl.BoolVarP(&f.timestamps, "timestamps", "t", false, "prefix each line with a timestamp")
	fl.DurationVar(&f.since, "since", 0, "only logs newer than this (e.g. 10m, 1h; 0 = no limit)")
	fl.StringVar(&f.node, "node", "", "node hint/override for container-id targets")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
	return cmd
}

func runLogs(cmd *cobra.Command, g *globalFlags, f *logsFlags, args []string) error {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ep, err := r.Resolve(ctx, resolve.Request{Target: args[0], NodeHint: f.node})
	if err != nil {
		if amb, ok := err.(*resolve.AmbiguousError); ok {
			ep, err = pickCandidate(amb, false)
		}
		if err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
	}

	if err := streamLogs(ctx, cfg, *ep, logsParams{
		follow:         f.follow,
		tail:           f.tail,
		timestamps:     f.timestamps,
		since:          f.since,
		connectTimeout: f.connectTimeout,
	}, os.Stdout, os.Stderr); err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	return nil
}

// logsParams carries the Logs request options.
type logsParams struct {
	follow         bool
	tail           uint32
	timestamps     bool
	since          time.Duration
	connectTimeout time.Duration
}

// streamLogs dials the agent and streams the container's logs into stdout/stderr.
// It is shared by the `logs` command and the interactive `ui`. Returns nil when
// the stream ends or ctx is cancelled.
func streamLogs(ctx context.Context, cfg config.Config, ep resolve.Endpoint, p logsParams, stdout, stderr io.Writer) error {
	dctx, dcancel := context.WithTimeout(ctx, p.connectTimeout)
	conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
	dcancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).Logs(ctx, &pb.LogsRequest{
		ContainerId:  ep.ContainerID,
		Follow:       p.follow,
		Tail:         p.tail,
		Timestamps:   p.timestamps,
		SinceSeconds: uint32(p.since.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("open logs stream: %w", err)
	}

	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || errors.Is(err, context.Canceled) {
				return nil
			}
			if st, ok := status.FromError(err); ok {
				return fmt.Errorf("%s: %s", st.Code(), st.Message())
			}
			return err
		}
		switch pl := chunk.Payload.(type) {
		case *pb.LogChunk_Stdout:
			_, _ = stdout.Write(pl.Stdout)
		case *pb.LogChunk_Stderr:
			_, _ = stderr.Write(pl.Stderr)
		case *pb.LogChunk_Error:
			return fmt.Errorf("agent: %s", pl.Error)
		}
	}
}
