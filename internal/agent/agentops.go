package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/plugin/docker"
)

// containerLogsFunc adapts dockerPlugin's live Docker client into the shape
// [libp2ptransport.RegisterAgentOpsHandler] calls for a container_logs
// request — the exact same docker.Client.Logs the local HTTP log endpoints
// on the control plane already call, just reached over AgentOps instead of a
// direct API call when the container being watched is on this Agent's host.
// No second Docker integration; this is two lines of channel adaptation over
// the existing one.
func containerLogsFunc(dockerPlugin *docker.Plugin) libp2ptransport.ContainerLogsFunc {
	return func(ctx context.Context, containerID string, tail int, since time.Time, follow, timestamps bool) (<-chan libp2ptransport.LogLine, <-chan error, error) {
		client := dockerPlugin.DockerClient()
		if client == nil {
			return nil, nil, fmt.Errorf("docker is not available on this host")
		}

		lines, errCh := client.Logs(ctx, containerID, docker.LogOptions{
			Follow: follow, Tail: tail, Since: since, Timestamps: timestamps,
		})

		out := make(chan libp2ptransport.LogLine)
		go func() {
			defer close(out)
			for line := range lines {
				select {
				case out <- libp2ptransport.LogLine{Time: line.Time, Stream: string(line.Stream), Message: line.Message}:
				case <-ctx.Done():
					return
				}
			}
		}()

		return out, errCh, nil
	}
}
