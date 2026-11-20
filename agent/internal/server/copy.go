// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"math"
	"path"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// The two halves of `swarmexec cp` (CONTRACT.md §3.11). Both go through the
// Engine's archive endpoint, which is the whole point: it needs nothing inside
// the container — not even tar, which distroless and scratch images, the very
// images one wants a file out of, do not carry.
//
// They are NOT restricted to swarm task containers, unlike kill and inspect.
// Copying is the second half of exec, and exec reaches every container on the
// node; a copy restricted where an exec is not would only push the operator
// back to `exec -- tar`. What separates the two directions is the permission:
// "copy.from" reads, "copy.to" writes into a container's filesystem.

const (
	// copyChunkSize is the payload per message. gRPC's default message cap is
	// 4 MiB; 256 KiB keeps a progress line moving and the agent's buffers small.
	copyChunkSize = 256 << 10

	// defaultCopyMaxBytes is the cap when the client names none: a copy is a
	// deliberate act on a known file, and "/" must fail rather than fill the
	// operator's disk.
	defaultCopyMaxBytes = 1 << 30 // 1 GiB
)

// CopyFrom streams a path out of a container as a tar archive. The first chunk
// carries the source's stat, so the client knows file-from-directory and the
// size before any payload arrives; every later chunk is payload.
func (s *Server) CopyFrom(req *pb.CopyFromRequest, stream pb.Agent_CopyFromServer) error {
	id, src := req.GetContainerId(), req.GetPath()
	if id == "" || src == "" {
		return status.Error(codes.InvalidArgument, "CopyFromRequest requires container_id and path")
	}
	ctx := stream.Context()

	a, err := s.authorize(ctx, auth.Request{Action: "copy.from", ContainerID: id, Path: src})
	if err != nil {
		return err
	}
	identity, service := a.Identity, a.Service

	start := time.Now()
	s.audit.CopyStart(identity, id, service, "from", src, peerAddr(ctx))
	sent := int64(0)
	fail := func(err error) error {
		s.audit.CopyEnd(identity, id, "from", time.Since(start), sent, false, status.Convert(err).Message())
		return err
	}

	// Resolve a symlinked source first when asked: the Engine would otherwise
	// hand back the link itself, which is rarely what `cp -L` means.
	if req.GetFollowLink() {
		src, err = s.resolveLink(ctx, id, src)
		if err != nil {
			return fail(err)
		}
	}

	res, err := s.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: src})
	if err != nil {
		s.log.Error("CopyFromContainer failed", "container_id", id, "path", src, "err", err)
		return fail(dockerStatus(err, "copy from container"))
	}
	defer res.Content.Close()

	if err := stream.Send(&pb.CopyChunk{Payload: &pb.CopyChunk_Stat{Stat: pathStat(res.Stat)}}); err != nil {
		return fail(err)
	}

	limit := capBytes(req.GetMaxBytes())
	out := &tallyFrom(ctx).out
	buf := make([]byte, copyChunkSize)
	for {
		n, rerr := res.Content.Read(buf)
		if n > 0 {
			sent += int64(n)
			if sent > limit {
				return fail(status.Errorf(codes.ResourceExhausted,
					"source exceeds the %d-byte limit; name a narrower path or raise --max-size", limit))
			}
			// The message takes ownership of these bytes, so they are copied
			// out of the reused buffer first (CONTRACT.md §6).
			payload := make([]byte, n)
			copy(payload, buf[:n])
			if err := stream.Send(&pb.CopyChunk{Payload: &pb.CopyChunk_Data{Data: payload}}); err != nil {
				return fail(err)
			}
			out.Add(int64(n))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			if ctx.Err() != nil { // the client went away: a normal end
				return fail(status.FromContextError(ctx.Err()).Err())
			}
			return fail(status.Errorf(codes.Internal, "read archive: %v", rerr))
		}
	}

	s.audit.CopyEnd(identity, id, "from", time.Since(start), sent, true, "")
	return nil
}

// StatPath answers what the Engine knows about one path inside a container.
// Authorized as "copy.from": it is a read of the container's filesystem, even
// though it moves no bytes. A path that is not there is reported as
// exists=false rather than as an error — the caller is asking a question.
func (s *Server) StatPath(ctx context.Context, req *pb.StatPathRequest) (*pb.StatPathResponse, error) {
	id, p := req.GetContainerId(), req.GetPath()
	if id == "" || p == "" {
		return nil, status.Error(codes.InvalidArgument, "StatPathRequest requires container_id and path")
	}
	if _, err := s.authorize(ctx, auth.Request{Action: "copy.from", ContainerID: id, Path: p}); err != nil {
		return nil, err
	}
	res, err := s.docker.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: p})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return &pb.StatPathResponse{Exists: false}, nil
		}
		return nil, dockerStatus(err, "stat path in container")
	}
	return &pb.StatPathResponse{Exists: true, Stat: pathStat(res.Stat)}, nil
}

// CopyTo extracts a tar stream into a container. The first message carries the
// destination, every later one payload; the Engine does the extracting, so its
// own refusal to replace a directory with a file still applies.
func (s *Server) CopyTo(stream pb.Agent_CopyToServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "CopyTo stream closed before its StartCopyTo")
		}
		return err
	}
	st := first.GetStart()
	if st == nil {
		return status.Error(codes.InvalidArgument, "the first CopyToMessage must carry a StartCopyTo")
	}
	id, dst := st.GetContainerId(), st.GetDestPath()
	if id == "" || dst == "" {
		return status.Error(codes.InvalidArgument, "StartCopyTo requires container_id and dest_path")
	}

	a, err := s.authorize(ctx, auth.Request{Action: "copy.to", ContainerID: id, Path: dst})
	if err != nil {
		return err
	}
	identity, service := a.Identity, a.Service

	start := time.Now()
	s.audit.CopyStart(identity, id, service, "to", dst, peerAddr(ctx))

	r := &copyToReader{stream: stream, limit: capBytes(st.GetMaxBytes()), count: &tallyFrom(ctx).in}
	_, cerr := s.docker.CopyToContainer(ctx, id, client.CopyToContainerOptions{
		DestinationPath:           dst,
		Content:                   r,
		CopyUIDGID:                st.GetCopyUidGid(),
		AllowOverwriteDirWithFile: st.GetAllowOverwriteDirWithFile(),
	})
	// The reader's own failure (a cap hit, a broken client stream) is the real
	// cause; the Engine only reports that its body ended early.
	if r.err != nil {
		cerr = r.err
	} else if cerr != nil {
		s.log.Error("CopyToContainer failed", "container_id", id, "path", dst, "err", cerr)
		cerr = dockerStatus(cerr, "copy to container")
	}
	if cerr != nil {
		s.audit.CopyEnd(identity, id, "to", time.Since(start), r.read, false, status.Convert(cerr).Message())
		return cerr
	}

	s.audit.CopyEnd(identity, id, "to", time.Since(start), r.read, true, "")
	return stream.SendAndClose(&pb.CopyToResponse{BytesWritten: r.read})
}

// copyToReader turns the client's payload messages into the io.Reader the
// Engine's archive endpoint consumes, counting bytes and enforcing the cap. A
// failure is kept in err because the Engine's call only reports that the body
// ended, not why.
type copyToReader struct {
	stream pb.Agent_CopyToServer
	limit  int64
	read   int64
	rest   []byte
	err    error
	count  *atomic.Int64
}

func (r *copyToReader) Read(p []byte) (int, error) {
	for len(r.rest) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			r.err = err
			return 0, err
		}
		if msg.GetStart() != nil {
			r.err = status.Error(codes.InvalidArgument, "only the first CopyToMessage may carry a StartCopyTo")
			return 0, r.err
		}
		data := msg.GetData()
		r.read += int64(len(data))
		if r.read > r.limit {
			r.err = status.Errorf(codes.ResourceExhausted,
				"payload exceeds the %d-byte limit; copy less or raise --max-size", r.limit)
			return 0, r.err
		}
		r.rest = data
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	r.count.Add(int64(n))
	return n, nil
}

// resolveLink re-stats a symlinked source so the copy follows it. A path that
// is not a link is returned unchanged, so -L on an ordinary file is a no-op.
func (s *Server) resolveLink(ctx context.Context, id, src string) (string, error) {
	res, err := s.docker.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: src})
	if err != nil {
		return "", dockerStatus(err, "stat path in container")
	}
	target := res.Stat.LinkTarget
	if target == "" {
		return src, nil
	}
	if !path.IsAbs(target) {
		target = path.Join(path.Dir(src), target)
	}
	return target, nil
}

// pathStat is the Engine's stat on the wire.
func pathStat(st container.PathStat) *pb.CopyStat {
	return &pb.CopyStat{
		Name:       st.Name,
		Size:       st.Size,
		Mode:       uint32(st.Mode),
		IsDir:      st.Mode.IsDir(),
		LinkTarget: st.LinkTarget,
		MtimeUnix:  st.Mtime.Unix(),
	}
}

// capBytes is the client's limit, or the agent's default when it named none.
// A client that names a larger one gets it: it is an authenticated, authorized
// operator naming a size for one copy, and the agent's default exists to stop a
// mistake ("/"), not to argue with a deliberate number. int64 overflow is the
// one thing refused.
func capBytes(want uint64) int64 {
	if want == 0 || want > uint64(math.MaxInt64) {
		return defaultCopyMaxBytes
	}
	return int64(want)
}
