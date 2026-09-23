// Package reaper holds the background workers that enforce the two timers the
// product sells to users. The files here are deliberately kept in THREE
// separate files: the idle reaper's action (hibernate) is fully reversible,
// the expiry reaper's action (destroy) is not, and warnings are a third, side
// effect-free concern. Merge them only over your strong objection.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/api-sandbox-links/backend/internal/cache"
	"github.com/api-sandbox-links/backend/internal/db"
	"github.com/api-sandbox-links/backend/internal/orchestrator"
)

// IdleReaper hibernates (stops) running containers that have received no
// request within their idle timeout. Reversible: the wake middleware restarts
// the container on the next request. Hibernation never deletes anything.
type IdleReaper struct {
	store    *db.Store
	cache    *cache.Redis
	orch     *orchestrator.Orchestrator
	logger   *slog.Logger
	interval time.Duration
}

// NewIdleReaper constructs the idle timer worker.
func NewIdleReaper(store *db.Store, cache *cache.Redis, orch *orchestrator.Orchestrator, logger *slog.Logger, interval time.Duration) *IdleReaper {
	return &IdleReaper{store: store, cache: cache, orch: orch, logger: logger, interval: interval}
}

// Run ticks every interval and hibernates any overdue running sandbox. It is
// cancellable via ctx (the server stops it cleanly on shutdown).
func (r *IdleReaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	r.tick(ctx) // check immediately on boot too
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick is one pass: consider every sandbox currently marked "running".
func (r *IdleReaper) tick(ctx context.Context) {
	active, err := r.store.ListActiveSandboxes()
	if err != nil {
		r.logger.Warn("idle-reaper: listing sandboxes", slog.String("err", err.Error()))
		return
	}
	for _, sb := range active {
		// Only running containers can hibernate; queued/building ones have no
		// container that is safe to stop yet.
		if sb.Status != db.StatusRunning || sb.ContainerID == "" {
			continue
		}
		if !r.cache.IsIdle(ctx, sb.ID) {
			continue
		}
		// No request touched this sandbox's marker for >= idle_timeout_seconds:
		// stop the container, keep every byte of data, mark it hibernated.
		if err := r.orch.StopContainer(ctx, sb.ContainerID); err != nil {
			r.logger.Warn("idle-reaper: stopping container", slog.String("sandbox", sb.ID))
			continue
		}
		if err := r.cache.Clear(ctx, sb.ID); err != nil {
			r.logger.Debug("idle-reaper: clearing marker", slog.String("sandbox", sb.ID))
		}
		r.orch.StopStatsCollector(sb.ID)
		if err := r.store.UpdateSandbox(sb.ID, map[string]any{
			"status": string(db.StatusHibernated),
		}); err != nil {
			r.logger.Warn("idle-reaper: marking hibernated", slog.String("sandbox", sb.ID))
		}
		r.logger.Info("idle-reaper: hibernated sandbox", slog.String("sandbox", sb.ID))
	}
}