package agent

import (
	"testing"
	"time"

	"github.com/hexane/atlas/internal/plugin/docker"
)

// #8: Docker unavailable on this host (Init never ran / detection failed)
// must surface as an error the AgentOps handler can turn into an error
// frame, not a nil-pointer panic or a silently empty stream.
func TestContainerLogsFuncErrorsWhenDockerUnavailable(t *testing.T) {
	dockerPlugin := docker.New(docker.Options{}) // never Init'd: DockerClient() is nil

	fn := containerLogsFunc(dockerPlugin)
	_, _, err := fn(t.Context(), "c1", 100, time.Time{}, false, false)
	if err == nil {
		t.Fatal("expected an error when Docker is not available on this host")
	}
}
