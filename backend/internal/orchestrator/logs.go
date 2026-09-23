package orchestrator

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
)

// DockerLogs opens a following stdout+stderr log stream for a container,
// returning the multiplexed reader. Callers own the returned ReadCloser and
// must close it. The stream stays open while the container runs (Follow=true)
// and data is demultiplexed by the caller (see api.streamLogFrames).
func (o *Orchestrator) DockerLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	reader, err := o.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "200",
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: opening container logs: %w", err)
	}
	return reader, nil
}