package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/api-sandbox-links/backend/internal/cache"
	"github.com/api-sandbox-links/backend/internal/db"
	"github.com/docker/docker/api/types/container"
)

// ResourceSample is one normalized resource measurement for a sandbox at one
// instant. CPUPercent is a real percentage derived from deltas between two
// Docker samples — see computeCPUPercent. NetworkRxBytes and NetworkTxBytes are
// bytes moved *since the previous sample*, not lifetime totals, so they read as
// a rate against the sample cadence.
type ResourceSample struct {
	SandboxID        string
	CPUPercent       float64
	MemoryUsedBytes  int64
	MemoryLimitBytes int64
	NetworkRxBytes   int64
	NetworkTxBytes   int64
	RecordedAt       time.Time
}

// computeCPUPercent derives a CPU percentage from two samples of Docker's
// counters. Docker never reports a percentage: cpu_usage.total_usage and
// system_cpu_usage are monotonic nanosecond counters, so only the *difference*
// between two samples is a rate. Reading the absolute totals instead yields
// lifetime-average utilisation — a plausible-looking number that is wrong by
// orders of magnitude right after a container starts, which is the single most
// common mistake when consuming Docker stats.
//
// A predecessor that carries no readings at all means Docker had nothing to
// report for the previous interval, which is what the first frame of a fresh
// stats stream looks like. That case must return 0 rather than be divided
// through: the deltas would span the container's entire lifetime, reproducing
// the very error this function exists to prevent. Checking for it on the
// predecessor's values is what catches it — a system-delta check does not, since
// subtracting zero from the current total leaves a large, perfectly
// "divisible" delta.
func computeCPUPercent(prev, cur *container.CPUStats) float64 {
	if prev == nil || cur == nil {
		return 0
	}
	if prev.SystemUsage == 0 || prev.CPUUsage.TotalUsage == 0 {
		return 0
	}
	cpuDelta := float64(cur.CPUUsage.TotalUsage) - float64(prev.CPUUsage.TotalUsage)
	systemDelta := float64(cur.SystemUsage) - float64(prev.SystemUsage)
	if systemDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	// The multiplier is how many CPUs the container may actually use, so the
	// order of these two matters whenever they disagree. OnlineCPUs is the cpuset
	// the cgroup was given, which is what system_cpu_usage is measured against,
	// so it is authoritative; percpu_usage is only the fallback, matching the
	// reference implementation in Docker's own CLI. Preferring the length of
	// percpu_usage would inflate the result on a cpuset-limited container.
	numCPUs := int(cur.OnlineCPUs)
	if numCPUs == 0 {
		numCPUs = len(cur.CPUUsage.PercpuUsage)
	}
	if numCPUs == 0 {
		numCPUs = 1
	}
	return (cpuDelta / systemDelta) * float64(numCPUs) * 100.0
}

// memoryUsedBytes strips reclaimable page cache out of a container's reported
// memory. memory_stats.usage is the cgroup's whole footprint including cache, so
// reporting it raw makes a container that merely touched a lot of files look
// like it is about to hit its limit — and disagrees with what `docker stats`
// shows the user.
//
// cgroup v1 exposes the figure as stats.cache; cgroup v2 (the unified hierarchy,
// and the default on the Docker Engine 29 / cgroup v2 hosts this was developed
// against) reports stats.inactive_file, which is the same idea — reclaimable,
// and not written back under pressure. Only inactive_file is subtracted, never
// total_inactive_file: the latter includes active_file, which is genuinely
// resident.
func memoryUsedBytes(mem container.MemoryStats) int64 {
	used := int64(mem.Usage)
	if inactive, ok := mem.Stats["inactive_file"]; ok {
		used -= int64(inactive)
	} else if cached, ok := mem.Stats["cache"]; ok {
		used -= int64(cached)
	}
	// A cache figure larger than usage is only possible while a cgroup is being
	// torn down; fall back to the raw number rather than report negative usage.
	if used < 0 {
		return int64(mem.Usage)
	}
	return used
}

// networkBytes sums rx/tx across every interface Docker reports. A container
// routinely has more than one, and Docker makes no promise that the first entry
// is the one carrying traffic — picking a single named interface silently
// undercounts, or reads a flat zero.
func networkBytes(nets map[string]container.NetworkStats) (rx, tx uint64) {
	for _, nw := range nets {
		rx += nw.RxBytes
		tx += nw.TxBytes
	}
	return rx, tx
}

// StatsCollector streams a container's Docker stats, computes CPU percent via
// deltas, and fans results out to two sinks: live WebSocket subscribers and
// the Redis latest-sample key that the flush worker persists to Postgres.
type StatsCollector struct {
	orchestrator *Orchestrator
	sandboxID    string
	containerID  string

	mu          sync.RWMutex
	subscribers map[chan ResourceSample]struct{}
	lastSample  *ResourceSample
	cancel      context.CancelFunc
	// closed is set once, under mu, by close. It exists because both close and
	// Unsubscribe end up closing subscriber channels, and a WebSocket handler
	// that unsubscribes after the collector's stream has already ended would
	// otherwise close the same channel twice and panic.
	closed bool
}

// EnsureStatsCollector starts (idempotently) a collector for the sandbox's
// container. It returns the collector; callers managing their own lifecycle
// should call Stop on a freshly-created one.
func (o *Orchestrator) EnsureStatsCollector(sandboxID, containerID string) (*StatsCollector, error) {
	o.statsMu.Lock()
	defer o.statsMu.Unlock()
	if c, ok := o.collectors[sandboxID]; ok {
		return c, nil
	}
	c := &StatsCollector{
		orchestrator: o,
		sandboxID:    sandboxID,
		containerID:  containerID,
		subscribers:  map[chan ResourceSample]struct{}{},
	}
	o.collectors[sandboxID] = c
	go c.run()
	return c, nil
}

// Stop shuts the collector down; its Docker stats stream and goroutine end.
func (o *Orchestrator) StopStatsCollector(sandboxID string) {
	o.statsMu.Lock()
	defer o.statsMu.Unlock()
	if c, ok := o.collectors[sandboxID]; ok {
		c.close()
		delete(o.collectors, sandboxID)
	}
}

// run consumes the Docker stats JSON stream and forwards each computed sample
// to subscribers and Redis. Docker emits roughly one frame per second per
// container, which is the cadence we normalize to; the stream itself is read
// continuously because leaving frames unread backs up the daemon's pipe.
//
// It exits when the stream closes, the collector is closed, or the
// orchestrator's context is cancelled at server shutdown.
func (c *StatsCollector) run() {
	// Derived from the orchestrator's context, not context.Background(): a
	// collector must never outlive the process that opened it.
	ctx, cancel := context.WithCancel(c.orchestrator.ctx)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	defer cancel()
	defer c.orchestrator.StopStatsCollector(c.sandboxID)

	reader, err := c.orchestrator.docker.ContainerStats(ctx, c.containerID, true)
	if err != nil {
		c.orchestrator.logger.Warn("stats: cannot open stats stream",
			slog.String("sandbox", c.sandboxID), slog.String("err", err.Error()))
		return
	}
	defer reader.Body.Close()

	dec := json.NewDecoder(reader.Body)
	var prevRX, prevTX uint64
	havePrev := false

	for {
		var s container.StatsResponse
		if err := dec.Decode(&s); err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return
			}
			c.orchestrator.logger.Debug("stats: decode error (reopening stream)",
				slog.String("sandbox", c.sandboxID), slog.String("err", err.Error()))
			// A hiccup mid-stream shouldn't kill the collector, but the reopen
			// needs a fresh connection and a fresh decoder.
			reader.Body.Close()
			reader, err = c.orchestrator.docker.ContainerStats(ctx, c.containerID, true)
			if err != nil {
				return
			}
			dec = json.NewDecoder(reader.Body)
			continue
		}

		// Every sample already carries the previous one in PreCPUStats, which is
		// exactly the pair the delta formula needs. Only the first sample of a
		// stream has an empty PreCPUStats, and computeCPUPercent reports 0 for
		// it rather than dividing by a zero system delta.
		rx, tx := networkBytes(s.Networks)

		// Docker reports *cumulative* per-interface counters, so what a client
		// wants is a delta. Two cases have no delta to report, and both are
		// handled the same way rather than trusted to the arithmetic:
		//
		//   - the first sample of a stream, where there is no predecessor. This
		//     is not hypothetical: collectors are created lazily, on the first
		//     client to connect, so the very first frame for a long-running
		//     sandbox is its whole lifetime of traffic. Attributing that to one
		//     second would spike the network card to the lifetime total.
		//   - a counter reset, when an interface leaves the network and the sum
		//     shrinks. `rx - prevRX` on uint64 would wrap to ~18 exabytes and get
		//     persisted as a real measurement.
		//
		// Both report zero, which is what a delta-less sample actually means.
		var drx, dtx uint64
		if havePrev && rx >= prevRX && tx >= prevTX {
			drx, dtx = rx-prevRX, tx-prevTX
		}

		sample := ResourceSample{
			SandboxID:        c.sandboxID,
			CPUPercent:       computeCPUPercent(&s.PreCPUStats, &s.CPUStats),
			MemoryUsedBytes:  memoryUsedBytes(s.MemoryStats),
			MemoryLimitBytes: int64(s.MemoryStats.Limit),
			NetworkRxBytes:   int64(drx),
			NetworkTxBytes:   int64(dtx),
			RecordedAt:       time.Now(),
		}
		prevRX, prevTX, havePrev = rx, tx, true

		c.setLast(&sample)
		c.broadcast(sample)
		c.publish(ctx, sample)
	}
}

// publish writes the newest sample to Redis under a short-lived key. The flush
// worker reads that key on its own cadence, so the 1s live feed never becomes
// database traffic, and a collector that dies simply lets the key expire
// instead of serving a frozen reading as if it were live.
func (c *StatsCollector) publish(ctx context.Context, sample ResourceSample) {
	raw, err := cache.EncodeStatFrame(cache.StatFrame{
		SandboxID:        sample.SandboxID,
		CPUPercent:       float32(sample.CPUPercent),
		MemoryUsedBytes:  sample.MemoryUsedBytes,
		MemoryLimitBytes: sample.MemoryLimitBytes,
		NetworkRxBytes:   sample.NetworkRxBytes,
		NetworkTxBytes:   sample.NetworkTxBytes,
		RecordedAt:       sample.RecordedAt,
	})
	if err != nil {
		return
	}
	if err := c.orchestrator.cache.SetLatestStat(ctx, c.sandboxID, raw); err != nil {
		c.orchestrator.logger.Debug("stats: redis publish failed",
			slog.String("sandbox", c.sandboxID), slog.String("err", err.Error()))
	}
}

// Subscribe registers a channel that receives every subsequent sample.
func (c *StatsCollector) Subscribe() chan ResourceSample {
	ch := make(chan ResourceSample, 16)
	c.mu.Lock()
	c.subscribers[ch] = struct{}{}
	c.mu.Unlock()
	return ch
}

// Unsubscribe removes a previously-registered channel, closing it so the
// subscriber's receive loop sees the end of the stream.
//
// It is safe to call after close: the collector takes ownership of closing its
// channels, and a second close on an already-closed channel panics. A handler
// that returns *because* the stream ended would hit exactly that.
func (c *StatsCollector) Unsubscribe(ch chan ResourceSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if _, ok := c.subscribers[ch]; !ok {
		return
	}
	delete(c.subscribers, ch)
	close(ch)
}

// Last returns a copy of the most recent sample, or nil before the first one
// arrives. The pointer is not shared with the collector, so a caller cannot
// observe a later sample mutating the value it is holding.
func (c *StatsCollector) Last() *ResourceSample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastSample == nil {
		return nil
	}
	cp := *c.lastSample
	return &cp
}

func (c *StatsCollector) setLast(s *ResourceSample) {
	c.mu.Lock()
	c.lastSample = s
	c.mu.Unlock()
}

// broadcast offers a sample to every subscriber, dropping it for any whose
// buffer is full so one stalled browser cannot back-pressure the Docker stream.
//
// The sends happen under the write lock. That looks heavier than copying the
// channel set under a read lock and sending outside it, but copying first is
// exactly the bug this replaces: the gap between the copy and the send let
// Unsubscribe close a channel that was about to be written to, and a send on a
// closed channel panics and takes the whole server down. The lock is cheap here
// precisely because every send is non-blocking.
func (c *StatsCollector) broadcast(s ResourceSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for ch := range c.subscribers {
		select {
		case ch <- s:
		default:
			// Slow subscriber: drop the sample rather than block the collector.
		}
	}
}

// close ends every subscription and cancels the Docker stats stream. It is
// idempotent: run's deferred StopStatsCollector and a concurrent explicit stop
// both land here.
func (c *StatsCollector) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for ch := range c.subscribers {
		close(ch)
	}
	c.subscribers = map[chan ResourceSample]struct{}{}
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// --- flush worker ---

// StatsFlushInterval is how often the newest live sample is persisted to
// resource_snapshots. Samples arrive roughly every second but exist to drive a
// live readout; writing each one would triple the database's write rate for
// history that is only ever read at 10s resolution. Named so the
// write-rate-versus-history-resolution trade-off is tunable in one place.
const StatsFlushInterval = 10 * time.Second

// StatsFlushWorker periodically persists each sandbox's newest live sample from
// Redis into Postgres. It is cancellable via ctx and exists so the database is
// never on the hot path of live metrics.
type StatsFlushWorker struct {
	store    *db.Store
	cache    *cache.Redis
	logger   *slog.Logger
	interval time.Duration
}

// NewStatsFlushWorker builds a flush worker with the given cadence.
func NewStatsFlushWorker(store *db.Store, cache *cache.Redis, logger *slog.Logger, interval time.Duration) *StatsFlushWorker {
	return &StatsFlushWorker{store: store, cache: cache, logger: logger, interval: interval}
}

// Run loops until ctx is cancelled, flushing every interval.
func (w *StatsFlushWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	// Flush once immediately so a crash right after boot still persists data.
	w.flushAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.flushAll(ctx)
		}
	}
}

// flushAll persists the newest sample for every sandbox that currently has one.
//
// It reads a single Redis key per sandbox rather than draining a per-sample
// buffer: at StatsFlushInterval the key is always newer than the last flush, and
// reading one key per sandbox keeps the whole pass to one Postgres insert.
//
// Consecutive passes can occasionally record the same sample twice — if a
// collector restarts mid-interval — which is harmless: the duplicate carries
// identical values, and the history chart draws a flat segment rather than a
// visible artifact.
func (w *StatsFlushWorker) flushAll(ctx context.Context) {
	active, err := w.store.ListActiveSandboxes()
	if err != nil {
		w.logger.Warn("stats-flush: listing active sandboxes", slog.String("err", err.Error()))
		return
	}

	rows := make([]*db.ResourceSnapshot, 0, len(active))
	for _, sb := range active {
		if sb.ContainerID == "" {
			continue
		}
		raw, err := w.cache.LatestStat(ctx, sb.ID)
		if err != nil {
			w.logger.Warn("stats-flush: reading latest stat",
				slog.String("sandbox", sb.ID), slog.String("err", err.Error()))
			continue
		}
		if len(raw) == 0 {
			continue // no collector running for this sandbox
		}
		sample, err := cache.DecodeStatFrame(raw)
		if err != nil {
			// A corrupt sample is not worth failing the whole pass over.
			continue
		}
		rows = append(rows, &db.ResourceSnapshot{
			SandboxID:        sample.SandboxID,
			CPUPercent:       sample.CPUPercent,
			MemoryUsedBytes:  sample.MemoryUsedBytes,
			MemoryLimitBytes: sample.MemoryLimitBytes,
			NetworkRxBytes:   sample.NetworkRxBytes,
			NetworkTxBytes:   sample.NetworkTxBytes,
			RecordedAt:       sample.RecordedAt,
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := w.store.InsertResourceSnapshots(rows); err != nil {
		w.logger.Warn("stats-flush: inserting snapshots", slog.String("err", err.Error()))
	}
}
