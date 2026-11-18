// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"swarmexec/agent/agenttest"
	"swarmexec/internal/pb"
)

// framedLines is n stdout lines of lineLen bytes as one stdcopy stream.
func framedLines(n, lineLen int) []byte {
	line := append(bytes.Repeat([]byte("x"), lineLen-1), '\n')
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		buf.Write(agenttest.Frame(1, line))
	}
	return buf.Bytes()
}

// plainReader hides bytes.Reader's WriteTo so the copy loop reads in chunks,
// as it does from the Engine's response body.
type plainReader struct{ io.Reader }

// drainLogs runs one Logs call to its end and returns the message and byte counts.
func drainLogs(tb testing.TB, ctx context.Context, c pb.AgentClient) (msgs, n int) {
	tb.Helper()
	st, err := c.Logs(ctx, &pb.LogsRequest{ContainerId: "c1"})
	if err != nil {
		tb.Fatal(err)
	}
	for {
		m, err := st.Recv()
		if err != nil {
			return msgs, n
		}
		msgs++
		n += len(m.GetStdout())
	}
}

// BenchmarkLogsNonTTY is PRF-02: 20 000 lines of 120 bytes through the real
// agent over loopback gRPC (non-TTY, so one stdcopy frame per line).
func BenchmarkLogsNonTTY(b *testing.B) {
	body := framedLines(20_000, 120)
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1"}).
		SetLogStream("c1", func() io.ReadCloser { return io.NopCloser(plainReader{bytes.NewReader(body)}) })
	a := agenttest.Start(b, agenttest.Options{Secret: auditSecret, AllowLegacySecret: true, Docker: d})
	c, ctx := rawClient(b, a, legacyCreds("bench"))
	b.ReportAllocs()
	b.SetBytes(20_000 * 120)
	var msgs int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, n := drainLogs(b, ctx, c)
		if n != 20_000*120 {
			b.Fatalf("got %d bytes", n)
		}
		msgs = m
	}
	b.ReportMetric(float64(msgs), "msgs/op")
}

// A lone line must not wait for more output: with the stream otherwise idle
// (and open, as in `logs -f`) it arrives within the batching delay.
func TestLogsSingleLineArrivesPromptly(t *testing.T) {
	pr, pw := io.Pipe()
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1"}).
		SetLogStream("c1", func() io.ReadCloser { return pr })
	a := agenttest.Start(t, agenttest.Options{Secret: auditSecret, AllowLegacySecret: true, Docker: d})
	c, ctx := rawClient(t, a, legacyCreds("alice"))
	st, err := c.Logs(ctx, &pb.LogsRequest{ContainerId: "c1", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()

	type result struct {
		out string
		err error
		d   time.Duration
	}
	got := make(chan result, 1)
	start := time.Now()
	go func() {
		m, err := st.Recv()
		got <- result{string(m.GetStdout()), err, time.Since(start)}
	}()
	if _, err := pw.Write(agenttest.Frame(1, []byte("hello\n"))); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.err != nil || r.out != "hello\n" {
			t.Fatalf("Recv = %q, %v", r.out, r.err)
		}
		t.Logf("single line delivered after %v", r.d)
		if r.d > 500*time.Millisecond { // 5 ms bound plus generous CI slack
			t.Errorf("line took %v, want about 5ms", r.d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("line never arrived: the batcher waits for more output")
	}
}

// Small frames come out coalesced, in order, with stdout/stderr interleaving
// preserved and nothing lost.
func TestLogsBatchesLinesAndKeepsOrder(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(agenttest.Frame(1, []byte("a\n")))
	buf.Write(agenttest.Frame(1, []byte("b\n")))
	buf.Write(agenttest.Frame(2, []byte("E\n")))
	buf.Write(agenttest.Frame(1, []byte("c\n")))
	body := buf.Bytes()
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1"}).
		SetLogStream("c1", func() io.ReadCloser { return io.NopCloser(bytes.NewReader(body)) })
	a := agenttest.Start(t, agenttest.Options{Secret: auditSecret, AllowLegacySecret: true, Docker: d})
	c, ctx := rawClient(t, a, legacyCreds("alice"))
	st, err := c.Logs(ctx, &pb.LogsRequest{ContainerId: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for {
		m, err := st.Recv()
		if err != nil {
			break
		}
		if o := m.GetStdout(); o != nil {
			seq = append(seq, "o:"+string(o))
		}
		if e := m.GetStderr(); e != nil {
			seq = append(seq, "e:"+string(e))
		}
	}
	want := []string{"o:a\nb\n", "e:E\n", "o:c\n"}
	if len(seq) != len(want) {
		t.Fatalf("messages = %q, want %q", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("messages = %q, want %q", seq, want)
		}
	}
}
