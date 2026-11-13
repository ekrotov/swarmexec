// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The health-check timings exist in four places: here (init's spec), the
// image's HEALTHCHECK, and both stack files. The image is what an image-only
// update brings, the spec what init writes; if they drifted, an agent's health
// behaviour would depend on how it was last deployed.
func TestHealthTimingsMatchImageAndStacks(t *testing.T) {
	dockerfile, err := os.ReadFile("../../agent/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`HEALTHCHECK --interval=%s --timeout=%s --start-period=%s --retries=%d CMD ["/agent", "-healthcheck"]`,
		HealthInterval, HealthTimeout, HealthStartPeriod, HealthRetries)
	if !strings.Contains(string(dockerfile), want+"\n") {
		t.Errorf("agent/Dockerfile has no\n  %s", want)
	}

	for _, f := range []string{"../../agent/deploy/agent-stack.yml", "../../agent/deploy/agent-stack-selfsigned.yml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		block := fmt.Sprintf("    healthcheck:\n      interval: %s\n      timeout: %s\n      start_period: %s\n      retries: %d\n",
			HealthInterval, HealthTimeout, HealthStartPeriod, HealthRetries)
		if !strings.Contains(string(b), block) {
			t.Errorf("%s has no healthcheck block\n%s", f, block)
		}
		// The command is the image's; a stack that named it would restart-loop
		// an agent pinned to an image without the probe.
		if regexp.MustCompile(`(?m)^\s+test:`).Match(b) {
			t.Errorf("%s names the probe command; leave it to the image", f)
		}
	}
}
