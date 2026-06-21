package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "swarmexec/internal/pb"
)

// echoAgent is a minimal in-process Agent: it requires a StartExec first, echoes
// stdin back as stdout, and on stdin EOF returns the exit code carried in the
// StartExec working_dir field (a test hook).
type echoAgent struct {
	pb.UnimplementedAgentServer
}

func (echoAgent) Exec(stream pb.Agent_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "first message must be StartExec")
	}
	for {
		m, err := stream.Recv()
		if err == io.EOF {
			return stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_ExitCode{ExitCode: 0}})
		}
		if err != nil {
			return err
		}
		if in := m.GetStdin(); in != nil {
			if err := stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: in}}); err != nil {
				return err
			}
		}
	}
}

func TestExecEndToEndOverGRPC(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterAgentServer(srv, echoAgent{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	stream, err := pb.NewAgentClient(conn).Exec(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code, runErr := Run(context.Background(), stream, Options{
		Start:  &pb.StartExec{ContainerId: "c1", Cmd: []string{"/bin/cat"}, Tty: false},
		Stdin:  strings.NewReader("ping-pong"),
		Stdout: &out,
		Stderr: &errb,
	})
	if runErr != nil {
		t.Fatalf("run error: %v", runErr)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if out.String() != "ping-pong" {
		t.Errorf("stdout = %q, want ping-pong", out.String())
	}
}
