package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/api-sandbox-links/backend/internal/db"
)

// warningTier couples a human-readable label to the number of seconds before
// expiry at which a warning should fire. Labels are also the dedupe key in
// the sandbox_lifetime_warnings table, so each tier is sent at most once.
type warningTier struct {
	label string
	seconds float64
}

// lifetimeThresholds returns the warning tiers that apply to a sandbox of the
// given lifetime. Long lifetimes get the fixed calendar-ish tiers (24h, 1h);
// every lifetime gets proportional tiers (50%, 25%) so that even a 1-hour
// sandbox warns at meaningful fractions. The final 5m tier always applies.

// thresholdsFor computes the per-lifetime warning schedule. Order is from
// earliest (largest lead time) to latest.
func thresholdsFor(lifetimeSeconds int64) []warningTier {
	var out []warningTier
	lifetime := float64(lifetimeSeconds)
	add := func(label string, sec float64) {
		if sec > 0 && sec < lifetime { // only tiers that actually precede expiry
			out = append(out, warningTier{label: label, seconds: sec})
		}
	}
	// Proportional tiers give short lifetimes their "meaningful fractions":
	add("p50", lifetime*0.5)
	add("p25", lifetime*0.25)
	// Calendar-ish tiers only make sense when the lifetime is clearly longer
	// than the lead time (else they'd duplicate the proportional warnings).
	if lifetime > 2*86400 {
		add("24h", 86400)
	}
	if lifetime > 2*3600 {
		add("1h", 3600)
	}
	add("5m", 300)
	// Sort by lead time descending (most urgent check first is irrelevant to
	// correctness; ordering just makes logs read naturally).
	return out
}

// WarningsWorker records which expiry warnings have been delivered so the
// frontend can render banners and the same message is never sent twice. Sending
// itself is a no-op beyond recording the tier: the UI reads these records
// through the API and shows the copy, which keeps this worker free of any
// messaging dependency.
type WarningsWorker struct {
	store    *db.Store
	logger   *slog.Logger
	interval time.Duration
}

// NewWarningsWorker builds the warnings worker.
func NewWarningsWorker(store *db.Store, logger *slog.Logger, interval time.Duration) *WarningsWorker {
	return &WarningsWorker{store: store, logger: logger, interval: interval}
}

// Run ticks periodically looking for sandboxes that have crossed a warning
// threshold since the last tick.
func (w *WarningsWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick evaluates every live sandbox against its warning schedule and records
// any tiers that have now been crossed.
func (w *WarningsWorker) tick(ctx context.Context) {
	active, err := w.store.ListActiveSandboxes()
	if err != nil {
		w.logger.Warn("warnings: listing sandboxes", slog.String("err", err.Error()))
		return
	}
	for _, sb := range active {
		remaining := time.Until(sb.ExpiresAt)
		if remaining <= 0 {
			continue // the expiry reaper owns destruction
		}
		for _, tier := range thresholdsFor(sb.LifetimeSeconds) {
			if remaining.Seconds() > tier.seconds {
				continue // not yet within this tier's window
			}
			sent, err := w.store.HasWarningSent(sb.ID, tier.label)
			if err != nil {
				w.logger.Warn("warnings: checking sent state", slog.String("sandbox", sb.ID))
				continue
			}
			if sent {
				continue
			}
			if err := w.store.MarkWarningSent(sb.ID, tier.label); err != nil {
				w.logger.Warn("warnings: recording", slog.String("sandbox", sb.ID))
				continue
			}
			w.logger.Info("warnings: recorded",
				slog.String("sandbox", sb.ID), slog.String("tier", tier.label))
		}
	}
}