package reaper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/api-sandbox-links/backend/internal/config"
	"github.com/api-sandbox-links/backend/internal/db"
	"github.com/api-sandbox-links/backend/internal/orchestrator"
)

// ExpiryReaper enforces the irreversible half of the timer story: when a
// sandbox's expires_at passes, it is torn down completely — app container,
// sidecar DB containers, their data volumes, the private network — and its row
// is left as a tombstone (status 'expired', destroyed_at set) so the user can
// still see it and recreate it. It shares NO code with IdleReaper by design.
type ExpiryReaper struct {
	store    *db.Store
	orch     *orchestrator.Orchestrator
	cfg      *config.Config
	logger   *slog.Logger
	interval time.Duration
}

// NewExpiryReaper constructs the destroyer worker.
func NewExpiryReaper(store *db.Store, orch *orchestrator.Orchestrator, cfg *config.Config, logger *slog.Logger, interval time.Duration) *ExpiryReaper {
	return &ExpiryReaper{store: store, orch: orch, cfg: cfg, logger: logger, interval: interval}
}

// Run ticks every interval and destroys every due sandbox. Cancellable via ctx.
func (r *ExpiryReaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick finds due-destroyed sandboxes and permanently removes their runtime.
func (r *ExpiryReaper) tick(ctx context.Context) {
	due, err := r.store.ListDuedExpiredSandboxes(time.Now())
	if err != nil {
		r.logger.Warn("expiry-reaper: listing due sandboxes", slog.String("err", err.Error()))
		return
	}
	for _, sb := range due {
		// If the sandbox never got past being queued, there is nothing running
		// to tear down — just record the tombstone.
		if sb.ContainerID != "" {
			if err := r.orch.TeardownSandbox(ctx, sb); err != nil {
				r.logger.Warn("expiry-reaper: teardown failed (will retry next tick)",
					slog.String("sandbox", sb.ID), slog.String("err", err.Error()))
				continue
			}
		}
		// Remove the on-disk working copy too: destruction is meant to be total.
		_ = os.RemoveAll(filepath.Join(r.cfg.SandboxWorkDir, sb.Subdomain))

		now := time.Now()
		if err := r.store.UpdateSandbox(sb.ID, map[string]any{
			"status":             string(db.StatusExpired),
			"destroyed_at":       now,
			"destruction_reason": string(db.DestroyLifetimeExpired),
			"container_id":       "",
			"image_tag":          "",
		}); err != nil {
			r.logger.Warn("expiry-reaper: marking sandbox expired",
				slog.String("sandbox", sb.ID), slog.String("err", err.Error()))
			continue
		}
		r.logger.Info("expiry-reaper: destroyed sandbox",
			slog.String("sandbox", sb.ID), slog.String("subdomain", sb.Subdomain))
	}
}