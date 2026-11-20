// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"io"
	"time"

	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// The three calls behind `swarmexec cp` (CONTRACT.md §3.11). Unlike the other
// container calls they are not bounded by containerCallTimeout: a copy is as
// long as the file is. Only the dial is bounded; the transfer runs under the
// caller's context, which is what Ctrl-C cancels.

// copyChunkSize is the payload per message on the way in. It matches the
// agent's outbound chunk so both directions of a container-to-container copy
// move the same sized pieces.
const copyChunkSize = 256 << 10

// StatPath asks what the container has at p. The second result is false when
// the path is not there, which is an answer, not a failure: it is how the
// caller tells "write this file" from "write into this directory".
func (c *Client) StatPath(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration, p string) (*pb.CopyStat, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout+containerCallTimeout)
	defer cancel()
	conn, err := c.Conn(cctx, ep.DialHost)
	if err != nil {
		return nil, false, c.Explain(ctx, err)
	}
	defer conn.Close()
	resp, err := conn.Agent().StatPath(cctx, &pb.StatPathRequest{ContainerId: ep.ContainerID, Path: p})
	conn.done(err)
	if err != nil {
		return nil, false, c.Explain(ctx, err)
	}
	return resp.GetStat(), resp.GetExists(), nil
}

// CopyFromOptions is one read out of a container.
type CopyFromOptions struct {
	Path       string
	FollowLink bool
	MaxBytes   uint64
}

// CopyFrom streams the tar archive of opts.Path into w and returns the
// source's stat, which the agent sends before any payload. progress, when set,
// is called with the running byte count.
func (c *Client) CopyFrom(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration, opts CopyFromOptions, w io.Writer, progress func(int64)) (*pb.CopyStat, int64, error) {
	dctx, dcancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, ep.DialHost)
	dcancel()
	if err != nil {
		return nil, 0, c.Explain(ctx, err)
	}
	defer conn.Close()

	stream, err := conn.Agent().CopyFrom(ctx, &pb.CopyFromRequest{
		ContainerId: ep.ContainerID,
		Path:        opts.Path,
		FollowLink:  opts.FollowLink,
		MaxBytes:    opts.MaxBytes,
	})
	if err != nil {
		conn.done(err)
		return nil, 0, c.Explain(ctx, err)
	}

	var stat *pb.CopyStat
	var n int64
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				conn.done(nil)
				return stat, n, nil
			}
			conn.done(rerr)
			return stat, n, c.Explain(ctx, rerr)
		}
		if e := chunk.GetError(); e != "" {
			return stat, n, errors.New(e)
		}
		if s := chunk.GetStat(); s != nil {
			stat = s
			continue
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		if _, werr := w.Write(data); werr != nil {
			return stat, n, werr
		}
		n += int64(len(data))
		if progress != nil {
			progress(n)
		}
	}
}

// CopyToOptions is one write into a container.
type CopyToOptions struct {
	DestDir      string
	CopyUIDGID   bool
	AllowReplace bool
	MaxBytes     uint64
}

// CopyTo streams the tar archive in r into the container and returns how many
// payload bytes the agent handed to the Engine.
func (c *Client) CopyTo(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration, opts CopyToOptions, r io.Reader, progress func(int64)) (int64, error) {
	dctx, dcancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, ep.DialHost)
	dcancel()
	if err != nil {
		return 0, c.Explain(ctx, err)
	}
	defer conn.Close()

	stream, err := conn.Agent().CopyTo(ctx)
	if err != nil {
		conn.done(err)
		return 0, c.Explain(ctx, err)
	}
	start := &pb.StartCopyTo{
		ContainerId:               ep.ContainerID,
		DestPath:                  opts.DestDir,
		CopyUidGid:                opts.CopyUIDGID,
		AllowOverwriteDirWithFile: opts.AllowReplace,
		MaxBytes:                  opts.MaxBytes,
	}
	if err := stream.Send(&pb.CopyToMessage{Payload: &pb.CopyToMessage_Start{Start: start}}); err != nil {
		return 0, c.sendFailure(ctx, conn, stream, err)
	}

	buf := make([]byte, copyChunkSize)
	var n int64
	for {
		read, rerr := r.Read(buf)
		if read > 0 {
			// The message takes ownership of these bytes (CONTRACT.md §6).
			payload := make([]byte, read)
			copy(payload, buf[:read])
			if serr := stream.Send(&pb.CopyToMessage{Payload: &pb.CopyToMessage_Data{Data: payload}}); serr != nil {
				return n, c.sendFailure(ctx, conn, stream, serr)
			}
			n += int64(read)
			if progress != nil {
				progress(n)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			_ = stream.CloseSend()
			conn.done(rerr)
			return n, rerr
		}
	}

	resp, err := stream.CloseAndRecv()
	conn.done(err)
	if err != nil {
		return n, c.Explain(ctx, err)
	}
	return resp.GetBytesWritten(), nil
}

// sendFailure turns a failed Send into the agent's real reason. A stream whose
// server side has already failed reports the local "EOF" on Send; the status
// that says why only comes back from CloseAndRecv.
func (c *Client) sendFailure(ctx context.Context, conn *Conn, stream pb.Agent_CopyToClient, sendErr error) error {
	if _, err := stream.CloseAndRecv(); err != nil {
		conn.done(err)
		return c.Explain(ctx, err)
	}
	conn.done(sendErr)
	return c.Explain(ctx, sendErr)
}
