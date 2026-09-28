package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/api-sandbox-links/backend/internal/db"
)

const (
	// SnapshotRetention is the age at which a resource sample is discarded.
	// Nothing the API exposes reads further back than 7d, so anything older is
	// pure storage cost in a table that only ever grows.
	SnapshotRetention = 7 * 24 * time.Hour
	// SnapshotSweepInterval is how often the retention sweep runs. Hourly is
	// ample: the table holds days of data, and deleting a few extra hours of it
	// is not observable anywhere.
	SnapshotSweepInterval = time.Hour
)

// SnapshotReaper enforces the retention window on resource_snapshots.
//
// It is the fourth kind of background job in this package, and unlike the other
// three it touches no user-visible state: it only bounds the growth of the
// metrics history table. It lives here rather than in the orchestrator because
// it needs nothing but the store — no cache, no Docker, no config.
type SnapshotReaper struct {
	store     *db.Store
	logger    *slog.Logger
	interval  time.Duration
	retention time.Duration
}

// NewSnapshotReaper constructs the retention worker.
func NewSnapshotReaper(store *db.Store, logger *slog.Logger, interval, retention time.Duration) *SnapshotReaper {
	return &SnapshotReaper{store: store, logger: logger, interval: interval, retention: retention}
}

// Run ticks every interval and trims expired samples. Cancellable via ctx.
func (r *SnapshotReaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick deletes everything past the retention window.
//
// Unlike the idle and expiry reapers this one does not sweep on boot: a freshly
// started server has nothing to trim yet, and the first pass is due an hour in.
// That also keeps a rolling restart from issuing a large delete on every deploy.
func (r *SnapshotReaper) tick(_ context.Context) {
	cutoff := time.Now().Add(-r.retention)
	deleted, err := r.store.DeleteResourceSnapshotsBefore(cutoff)
	if err != nil {
		r.logger.Warn("snapshot-reaper: deleting expired samples", slog.String("err", err.Error()))
		return
	}
	if deleted > 0 {
		r.logger.Info("snapshot-reaper: deleted expired samples",
			slog.Int64("deleted", deleted),
			slog.Time("cutoff", cutoff))
	}
}
