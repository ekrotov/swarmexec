package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/client"

	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
)

// newDockerClient builds a Docker SDK client for the manager API, honoring
// Docker CLI contexts (--context / $DOCKER_CONTEXT / the config's current
// context) including ssh:// endpoints — which the bare SDK's client.FromEnv
// does not support (REQUIREMENTS §2).
func newDockerClient(contextOverride string) (resolve.DockerClient, error) {
	host, err := dockerctx.ResolveHost(contextOverride)
	if err != nil {
		return nil, err
	}

	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if strings.HasPrefix(host, "ssh://") {
		// ssh endpoints need a connection helper (it tunnels the Docker API over
		// ssh, the same way `docker --context <ssh-ctx>` does).
		helper, err := connhelper.GetConnectionHelper(host)
		if err != nil {
			return nil, fmt.Errorf("set up ssh connection to %s: %w", host, err)
		}
		opts = append(opts,
			client.WithHTTPClient(&http.Client{Transport: &http.Transport{DialContext: helper.Dialer}}),
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		)
	} else {
		opts = append(opts, client.WithHost(host))
	}

	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to Docker manager API (%s): %w", host, err)
	}
	return c, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func uptime(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
