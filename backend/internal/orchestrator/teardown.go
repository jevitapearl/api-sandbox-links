package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/api-sandbox-links/backend/internal/db"
	"github.com/docker/docker/api/types/container"
)

// TeardownSandbox irreversibly removes everything the running sandbox owns:
// the app container, any sidecar database containers, their data volumes, and
// the private network. It is shared by the expiry reaper (lifetime_expired)
// and the manual "Destroy now" action (user_requested) — never by the idle
// reaper, whose hibernate action is reversible and must not touch volumes.
//
// The sandbox metadata row is deliberately NOT deleted here; the caller marks
// it destroyed (tombstone) afterward.
func (o *Orchestrator) TeardownSandbox(ctx context.Context, sandbox *db.Sandbox) error {
	containers, err := o.FindContainersBySandbox(ctx, sandbox.ID)
	if err != nil {
		return fmt.Errorf("orchestrator: listing sandbox containers for teardown: %w", err)
	}
	for _, c := range containers {
		o.StopStatsCollector(sandbox.ID)
		if err := o.docker.ContainerStop(ctx, c.ID, container.StopOptions{}); err != nil {
			o.logger.Warn("teardown: stopping container",
				slog.String("container", c.ID), slog.String("err", err.Error()))
		}
		if err := o.RemoveContainer(ctx, c.ID); err != nil {
			o.logger.Warn("teardown: removing container",
				slog.String("container", c.ID), slog.String("err", err.Error()))
		}
	}

	// Sidecar database volumes are the only durable per-sandbox storage; remove
	// them so nothing survives the destruction.
	dbs, err := o.store.ListSandboxDatabases(sandbox.ID)
	if err != nil {
		return fmt.Errorf("orchestrator: listing sandbox databases for teardown: %w", err)
	}
	for _, dbb := range dbs {
		if dbb.ContainerID != "" {
			o.StopStatsCollector(sandbox.ID)
			if err := o.RemoveContainer(ctx, dbb.ContainerID); err != nil {
				o.logger.Warn("teardown: removing db container",
					slog.String("container", dbb.ContainerID), slog.String("err", err.Error()))
			}
		}
		if err := o.RemoveVolume(ctx, volumeName(dbb.ID)); err != nil {
			o.logger.Warn("teardown: removing db volume",
				slog.String("db", dbb.ID), slog.String("err", err.Error()))
		}
	}

	if err := o.RemoveNetwork(ctx, sandbox.ID); err != nil {
		o.logger.Warn("teardown: removing network",
			slog.String("sandbox", sandbox.ID), slog.String("err", err.Error()))
	}
	return nil
}