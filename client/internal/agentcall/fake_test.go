// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/authmeta"
	"swarmexec/internal/pb"
)

// fakeAgent answers every RPC agentcall makes, checks the channel binding of
// each call against its OWN certificate, and counts what it serves.
type fakeAgent struct {
	pb.UnimplementedAgentServer
	secret string
	cert   tls.Certificate

	// unimplemented names RPCs this agent answers Unimplemented, like an agent
	// that predates them.
	unimplemented map[string]bool

	calls     atomic.Int64
	badBind   atomic.Int64 // calls whose binding did not match this agent's cert
	vols      []string
	execOut   string
	pruned    atomic.Int64
	removedMu sync.Mutex
	removed   []string
}

func (a *fakeAgent) check(ctx context.Context, rpc string) error {
	a.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	if want := authmeta.Bind(a.secret, a.cert.Certificate[0]); len(md.Get(authmeta.BindingKey)) != 1 || md.Get(authmeta.BindingKey)[0] != want {
		a.badBind.Add(1)
		return status.Error(codes.Unauthenticated, "binding does not match this connection")
	}
	if a.unimplemented[rpc] {
		return status.Errorf(codes.Unimplemented, "unknown method %s", rpc)
	}
	return nil
}

func (a *fakeAgent) Version(ctx context.Context, _ *pb.VersionRequest) (*pb.VersionResponse, error) {
	if err := a.check(ctx, "Version"); err != nil {
		return nil, err
	}
	return &pb.VersionResponse{Version: "v9", ProtoVersion: pb.ProtocolVersion}, nil
}

func (a *fakeAgent) Stats(ctx context.Context, _ *pb.StatsRequest) (*pb.StatsResponse, error) {
	if err := a.check(ctx, "Stats"); err != nil {
		return nil, err
	}
	return &pb.StatsResponse{CpuReady: true, NodeCpus: 4, NodeMemoryTotalBytes: 8 << 30, Stats: []*pb.ContainerStats{
		{ContainerId: "c-" + a.cert.Leaf.Subject.CommonName, CpuPercent: 50, MemoryBytes: 1 << 20, Health: "healthy"},
	}}, nil
}

func (a *fakeAgent) ListImages(ctx context.Context, _ *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	if err := a.check(ctx, "ListImages"); err != nil {
		return nil, err
	}
	return &pb.ListImagesResponse{TotalBytes: 100, DanglingBytes: 10, UnusedBytes: 20, Images: []*pb.ImageInfo{{}, {}}}, nil
}

func (a *fakeAgent) PruneImages(ctx context.Context, _ *pb.PruneImagesRequest) (*pb.PruneImagesResponse, error) {
	if err := a.check(ctx, "PruneImages"); err != nil {
		return nil, err
	}
	return &pb.PruneImagesResponse{ReclaimedBytes: 10, Deleted: []string{"x"}}, nil
}

func (a *fakeAgent) ReclaimStats(ctx context.Context, _ *pb.ReclaimStatsRequest) (*pb.ReclaimStatsResponse, error) {
	if err := a.check(ctx, "ReclaimStats"); err != nil {
		return nil, err
	}
	return &pb.ReclaimStatsResponse{}, nil
}

func (a *fakeAgent) Prune(ctx context.Context, _ *pb.PruneRequest) (*pb.PruneResponse, error) {
	if err := a.check(ctx, "Prune"); err != nil {
		return nil, err
	}
	a.pruned.Add(1)
	return &pb.PruneResponse{}, nil
}

func (a *fakeAgent) ListVolumes(ctx context.Context, req *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	if err := a.check(ctx, "ListVolumes"); err != nil {
		return nil, err
	}
	out := &pb.ListVolumesResponse{}
	for _, v := range a.vols {
		out.Volumes = append(out.Volumes, &pb.VolumeInfo{Name: v, Driver: "local", CreatedAt: "2026-01-02T03:04:05Z",
			SizeBytes: 1 << 20, SizeKnown: req.GetWithSize()})
	}
	return out, nil
}

func (a *fakeAgent) ListContainers(ctx context.Context, _ *pb.ListRequest) (*pb.ListResponse, error) {
	if err := a.check(ctx, "ListContainers"); err != nil {
		return nil, err
	}
	return &pb.ListResponse{Containers: []*pb.ContainerInfo{{Id: "abc", Name: "web.1", Service: "web", Volumes: a.vols, Ports: []uint32{80}}}}, nil
}

func (a *fakeAgent) RemoveVolume(ctx context.Context, req *pb.RemoveVolumeRequest) (*pb.RemoveVolumeResponse, error) {
	if err := a.check(ctx, "RemoveVolume"); err != nil {
		return nil, err
	}
	a.removedMu.Lock()
	a.removed = append(a.removed, req.GetName())
	a.removedMu.Unlock()
	return &pb.RemoveVolumeResponse{}, nil
}

func (a *fakeAgent) CreateVolume(ctx context.Context, _ *pb.CreateVolumeRequest) (*pb.CreateVolumeResponse, error) {
	if err := a.check(ctx, "CreateVolume"); err != nil {
		return nil, err
	}
	return &pb.CreateVolumeResponse{}, nil
}

func (a *fakeAgent) Exec(stream pb.Agent_ExecServer) error {
	if err := a.check(stream.Context(), "Exec"); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	cmd := strings.Join(first.GetStart().GetCmd(), " ")
	out := a.execOut
	if out == "" {
		out = "ran: " + cmd
	}
	if err := stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: []byte(out)}}); err != nil {
		return err
	}
	return stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_ExitCode{ExitCode: 0}})
}

// fakeCluster is a set of fake agents, one per node host, reachable only
// through the config's ProxyDialer — the way agents behind an ssh bastion are.
// It counts dials (TCP connections, i.e. ssh channels over a bastion) and TLS
// handshakes at the source: in the dialer and in the servers' TLS config.
type fakeCluster struct {
	cfg        config.Config
	nodes      []resolve.Node
	agents     map[string]*fakeAgent // by host
	dials      atomic.Int64
	handshakes atomic.Int64
	servers    map[string]*grpc.Server
	addrs      map[string]string // "host:7000" -> listener address

	mu   sync.Mutex
	down map[string]bool // hosts that refuse connections
}

const fakeSecret = "s3cret"

func newFakeCluster(t *testing.T, hosts ...string) *fakeCluster {
	t.Helper()
	fc := &fakeCluster{agents: map[string]*fakeAgent{}, servers: map[string]*grpc.Server{}, addrs: map[string]string{}, down: map[string]bool{}}
	for _, h := range hosts {
		fc.start(t, h)
		fc.nodes = append(fc.nodes, resolve.Node{ID: "id-" + h, Name: h, DialHost: h})
	}
	fc.cfg = config.Config{
		Port:        7000,
		Insecure:    true,
		AgentSecret: fakeSecret,
		ProxyDialer: func(ctx context.Context, addr string) (net.Conn, error) {
			fc.dials.Add(1)
			fc.mu.Lock()
			real, ok := fc.addrs[addr]
			down := fc.down[addr]
			fc.mu.Unlock()
			if !ok || down {
				return nil, fmt.Errorf("dial %s: connection refused", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", real)
		},
	}
	return fc
}

// start (re)starts host's agent with a fresh certificate — what a restarted
// self-signed agent does.
func (fc *fakeCluster) start(t *testing.T, host string) *fakeAgent {
	t.Helper()
	key := net.JoinHostPort(host, "7000")
	fc.mu.Lock()
	prevAddr := fc.addrs[key]
	fc.mu.Unlock()
	listenOn := "127.0.0.1:0"
	if prevAddr != "" {
		listenOn = prevAddr // same address, as a restarted agent would be
	}
	var lis net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if lis, err = net.Listen("tcp", listenOn); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	cert := selfSigned(t, host)
	a := &fakeAgent{secret: fakeSecret, cert: cert, vols: []string{"data"}}
	if old := fc.agents[host]; old != nil {
		a.unimplemented = old.unimplemented
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			fc.handshakes.Add(1)
			return nil, nil
		},
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	pb.RegisterAgentServer(gs, a)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	fc.mu.Lock()
	fc.agents[host] = a
	fc.servers[host] = gs
	fc.addrs[key] = lis.Addr().String()
	fc.mu.Unlock()
	return a
}

// stop shuts host's agent down, closing its connections.
func (fc *fakeCluster) stop(host string) {
	fc.mu.Lock()
	gs := fc.servers[host]
	fc.mu.Unlock()
	gs.Stop()
}

func (fc *fakeCluster) setDown(host string, down bool) {
	fc.mu.Lock()
	fc.down[net.JoinHostPort(host, "7000")] = down
	fc.mu.Unlock()
}

func (fc *fakeCluster) totalCalls() int64 {
	var n int64
	for _, a := range fc.agents {
		n += a.calls.Load()
	}
	return n
}

func (fc *fakeCluster) badBindings() int64 {
	var n int64
	for _, a := range fc.agents {
		n += a.badBind.Load()
	}
	return n
}

func selfSigned(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{cn},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
