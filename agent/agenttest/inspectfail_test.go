// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/agenttest"
	"swarmexec/internal/pb"
)

// SEC-08: when the container cannot be inspected its service is unknown, so a
// rule scoped to that service cannot match and the default would decide. The
// agent refuses instead: no exec may start on data the policy could not read.
func TestAuthorize_FailsClosedWhenInspectFails(t *testing.T) {
	var ran atomic.Bool
	d := agenttest.NewDocker().
		AddContainer(agenttest.Container{ID: "c1", Service: "web"}).
		SetExec(func(_ []string, _ io.Reader, _, _ io.Writer) int { ran.Store(true); return 0 }).
		Fail("ContainerInspect", errors.New("daemon hiccup"))
	a := agenttest.Start(t, agenttest.Options{Secret: auditSecret, AllowLegacySecret: true, Docker: d,
		Policy: "default: allow\nrules:\n  - deny: [exec]\n    services: [web]\n    reason: web is off limits\n"})
	c, ctx := rawClient(t, a, legacyCreds("alice"))

	st, err := c.Exec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_Start{Start: &pb.StartExec{
		ContainerId: "c1", Cmd: []string{"id"},
	}}}); err != nil {
		t.Fatal(err)
	}
	_, err = st.Recv()
	if status.Code(err) == codes.OK || err == nil {
		t.Fatalf("exec with an unresolvable service was allowed (recv err = %v)", err)
	}
	if ran.Load() {
		t.Error("the command ran although the policy could not be applied")
	}
	// The decision itself was still audited.
	if len(a.AuditEvents("auth_decision")) == 0 {
		t.Error("no auth_decision record for the refused exec")
	}
}
