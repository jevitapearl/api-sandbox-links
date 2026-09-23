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

// StatsFrame is the live, computed resource snapshot for one sandbox at one
// instant. CPUPercent is a real percentage (0-100+) derived from deltas
// between two Docker samples — see computeCPUPercent.
type StatsFrame struct {
	SandboxID        string
	CPUPercent       float64
	MemoryUsedBytes  int64
	MemoryLimitBytes int64
	NetworkRXBytes   int64
	NetworkTXBytes   int64
	RecordedAt       time.Time
}

// computeCPUPercent turns one pair of CPU stats samples into a percentage.
// Docker gives cumulative nanoseconds of container and system CPU time; the
// *difference* between two samples over the *difference* in wall-time is the
// rate. Naively using absolute "cpu_total / system_time" is wrong because it
// measures lifetime utilisation, not current load — this delta calculation is
// the mandatory part of BUILD_PROMPT Phase D.
func computeCPUPercent(prev, cur *container.CPUStats, wallSeconds float64) float64 {
	if wallSeconds <= 0 || prev == nil || cur == nil {
		return 0
	}
	cpuDelta := float64(cur.CPUUsage.TotalUsage) - float64(prev.CPUUsage.TotalUsage)
	sysDelta := float64(cur.SystemUsage) - float64(prev.SystemUsage)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	// online_cpus normalizes so a single core pegged reports ~100%.
	online := float64(cur.OnlineCPUs)
	if online == 0 {
		online = 1
	}
	return (cpuDelta / sysDelta) * online * 100.0
}

// StatsCollector streams a container's Docker stats, computes CPU percent via
// deltas, and fans results out to two sinks: live WebSocket subscribers and
// the Redis buffer that a flush worker later drains into Postgres.
type StatsCollector struct {
	orchestrator *Orchestrator
	sandboxID    string
	containerID  string

	mu          sync.RWMutex
	subscribers map[chan StatsFrame]struct{}
	lastFrame   *StatsFrame
	cancel      context.CancelFunc
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
		subscribers:  map[chan StatsFrame]struct{}{},
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

// run consumes the Docker stats JSON stream in a loop and forwards each
// computed frame to subscribers and the Redis buffer. It exits when the
// stream closes or the collector is closed.
func (c *StatsCollector) run() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
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
	var prevCPU *container.CPUStats
	var prevWall time.Time
	var prevRX, prevTX uint64

	// Stats frames can carry lots of fields; decode leniently.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var s container.StatsResponse
		if err := dec.Decode(&s); err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return
			}
			c.orchestrator.logger.Debug("stats: decode error (retrying)",
				slog.String("sandbox", c.sandboxID), slog.String("err", err.Error()))
			// Stream hiccups shouldn't kill the collector; give the stream a
			// moment and reopen it below.
			time.Sleep(500 * time.Millisecond)
			reader.Body.Close()
			reader, err = c.orchestrator.docker.ContainerStats(ctx, c.containerID, true)
			if err != nil {
				return
			}
			dec = json.NewDecoder(reader.Body)
			continue
		}

		wall := s.Read.Sub(prevWall).Abs().Seconds()
		cpu := computeCPUPercent(prevCPU, &s.CPUStats, wall)

		rx, tx := s.Networks["eth0"].RxBytes, s.Networks["eth0"].TxBytes
		if rx == 0 && len(s.Networks) > 0 {
			// Summarize all interfaces if eth0 is absent/unnamed.
			for _, nw := range s.Networks {
				rx += nw.RxBytes
				tx += nw.TxBytes
			}
		}
		deltaRX, deltaTX := rx-prevRX, tx-prevTX
		prevRX, prevTX = rx, tx

		frame := StatsFrame{
			SandboxID:        c.sandboxID,
			CPUPercent:       cpu,
			MemoryUsedBytes:  int64(s.MemoryStats.Usage),
			MemoryLimitBytes: int64(s.MemoryStats.Limit),
			NetworkRXBytes:   int64(deltaRX),
			NetworkTXBytes:   int64(deltaTX),
			RecordedAt:       time.Now(),
		}
		prevCPU = &s.CPUStats
		prevWall = s.Read

		c.setLast(&frame)
		c.broadcast(frame)
		c.buffer(ctx, frame)

		select {
		case <-ticker.C:
		default:
		}
	}
}

// buffer writes a frame into the sandbox's Redis stats list (capped so the
// live stat stays fresh and the flush worker drains the tail).
func (c *StatsCollector) buffer(ctx context.Context, f StatsFrame) {
	frame := cache.StatFrame{
		SandboxID:        f.SandboxID,
		CPUPercent:       float32(f.CPUPercent),
		MemoryUsedBytes:  f.MemoryUsedBytes,
		MemoryLimitBytes: f.MemoryLimitBytes,
		NetworkRXBytes:   f.NetworkRXBytes,
		NetworkTXBytes:   f.NetworkTXBytes,
		RecordedAt:       f.RecordedAt,
	}
	raw, err := cache.EncodeStatFrame(frame)
	if err != nil {
		return
	}
	// Keep only a short tail; older frames are drained by the flush worker.
	if err := c.orchestrator.cache.PushStat(ctx, c.sandboxID, raw, 500); err != nil {
		c.orchestrator.logger.Debug("stats: redis buffer write failed",
			slog.String("sandbox", c.sandboxID), slog.String("err", err.Error()))
	}
}

// Subscribe registers a channel that receives every subsequent live frame.
func (c *StatsCollector) Subscribe() chan StatsFrame {
	ch := make(chan StatsFrame, 16)
	c.mu.Lock()
	c.subscribers[ch] = struct{}{}
	c.mu.Unlock()
	return ch
}

// Unsubscribe removes a previously-registered channel.
func (c *StatsCollector) Unsubscribe(ch chan StatsFrame) {
	c.mu.Lock()
	delete(c.subscribers, ch)
	close(ch)
	c.mu.Unlock()
}

// Last returns the most recent frame, or nil before the first sample.
func (c *StatsCollector) Last() *StatsFrame {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastFrame
}

func (c *StatsCollector) setLast(f *StatsFrame) {
	c.mu.Lock()
	c.lastFrame = f
	c.mu.Unlock()
}

func (c *StatsCollector) broadcast(f StatsFrame) {
	c.mu.RLock()
	subs := make([]chan StatsFrame, 0, len(c.subscribers))
	for ch := range c.subscribers {
		subs = append(subs, ch)
	}
	c.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- f:
		default:
			// Slow subscriber: drop the frame rather than block the collector.
		}
	}
}

func (c *StatsCollector) close() {
	c.mu.Lock()
	for ch := range c.subscribers {
		close(ch)
	}
	c.subscribers = map[chan StatsFrame]struct{}{}
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
}

// --- flush worker ---

// StatsFlushWorker periodically drains each active sandbox's Redis stats
// buffer into Postgres resource_snapshots. It is cancellable via ctx and
// exists so the DB is never on the hot path of live metrics.
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

// flushAll enumerates active sandboxes (from Postgres) and drains their Redis
// stat buffers into resource_snapshots in order.
func (w *StatsFlushWorker) flushAll(ctx context.Context) {
	var ids []string
	active, err := w.store.ListActiveSandboxes()
	if err != nil {
		w.logger.Warn("stats-flush: listing active sandboxes", slog.String("err", err.Error()))
		return
	}
	for _, sb := range active {
		if sb.ContainerID == "" {
			continue
		}
		ids = append(ids, sb.ID)
	}

	for _, id := range ids {
		rows, err := w.drainOne(ctx, id)
		if err != nil {
			w.logger.Warn("stats-flush: draining sandbox",
				slog.String("sandbox", id), slog.String("err", err.Error()))
			continue
		}
		if len(rows) > 0 {
			if err := w.store.InsertResourceSnapshots(rows); err != nil {
				w.logger.Warn("stats-flush: inserting snapshots",
					slog.String("sandbox", id), slog.String("err", err.Error()))
			}
		}
	}
}

func (w *StatsFlushWorker) drainOne(ctx context.Context, sandboxID string) ([]*db.ResourceSnapshot, error) {
	raws, err := w.cache.PopStatBatch(ctx, sandboxID, 500)
	if err != nil {
		return nil, err
	}
	var rows []*db.ResourceSnapshot
	for _, raw := range raws {
		f, err := cache.DecodeStatFrame(raw)
		if err != nil {
			continue // skip corrupt frames, don't fail the batch
		}
		rows = append(rows, &db.ResourceSnapshot{
			SandboxID:        f.SandboxID,
			CPUPercent:       f.CPUPercent,
			MemoryUsedBytes:  f.MemoryUsedBytes,
			MemoryLimitBytes: f.MemoryLimitBytes,
			NetworkRXBytes:   f.NetworkRXBytes,
			NetworkTXBytes:   f.NetworkTXBytes,
			RecordedAt:       f.RecordedAt,
		})
	}
	return rows, nil
}