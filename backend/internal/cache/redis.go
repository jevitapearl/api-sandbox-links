// Package cache wraps Redis, the platform's ephemeral/hot-state store.
//
// Redis deliberately holds only two kinds of data (see BUILD_PROMPT section 2):
//  1. last-request markers used by the idle-timeout reaper,
//  2. a short buffer of latest resource stats, flushed to Postgres in batches.
//
// Everything durable lives in Postgres; nothing in Redis is an authoritative
// record of anything.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the typed wrapper around a go-redis client.
type Redis struct {
	client *redis.Client
}

// New connects to Redis and verifies connectivity with a ping.
func New(ctx context.Context, url string) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("cache: parsing redis url: %w", err)
	}
	client := redis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("cache: unable to reach redis: %w", err)
	}
	return &Redis{client: client}, nil
}

// ---- last-request-at markers (idle timeout) -----------------------------
//
// Each request through the sandbox gateway calls Touch on the sandbox's key
// with a TTL equal to its idle timeout. The idle reaper hibernates a running
// sandbox when its key is absent — i.e. no request has bumped it within the
// timeout window. This TTL-as-timer trick means the reaper needs no stored
// timestamps and no clock skew issues.

func lastRequestKey(sandboxID string) string {
	return "lr:" + sandboxID
}

// Touch marks sandboxID as active for the next ttlSeconds. Called on every
// proxied request (hot path; keep cheap).
func (r *Redis) Touch(ctx context.Context, sandboxID string, ttlSeconds int) error {
	if err := r.client.Set(ctx, lastRequestKey(sandboxID), time.Now().Unix(), time.Duration(ttlSeconds)*time.Second).Err(); err != nil {
		return fmt.Errorf("cache: touching last-request marker: %w", err)
	}
	return nil
}

// IsIdle reports whether no request has touched the sandbox's marker, meaning
// it has likely been idle at least as long as its configured timeout.
func (r *Redis) IsIdle(ctx context.Context, sandboxID string) bool {
	n, err := r.client.Exists(ctx, lastRequestKey(sandboxID)).Result()
	if err != nil {
		return false
	}
	return n == 0
}

// Clear removes a sandbox's marker (used when hibernating so a fresh wake is
// counted as brand-new activity).
func (r *Redis) Clear(ctx context.Context, sandboxID string) error {
	return r.client.Del(ctx, lastRequestKey(sandboxID)).Err()
}

// ---- live stats buffer (resource dashboard) -----------------------------

func statsKey(sandboxID string) string {
	return "stats:" + sandboxID
}

// PushStat appends one encoded stats frame to the sandbox's buffer list and
// trims it to the newest maxLen entries.
func (r *Redis) PushStat(ctx context.Context, sandboxID string, frame []byte, maxLen int64) error {
	key := statsKey(sandboxID)
	pipe := r.client.TxPipeline()
	pipe.RPush(ctx, key, frame)
	pipe.LTrim(ctx, key, -maxLen, -1)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cache: buffering stat: %w", err)
	}
	return nil
}

// PopStatBatch atomically pops up to limit buffered frames and returns them in
// insertion order. Used by the flush worker to batch Redis frames into
// Postgres.
func (r *Redis) PopStatBatch(ctx context.Context, sandboxID string, limit int64) ([][]byte, error) {
	vals, err := r.client.LPopCount(ctx, statsKey(sandboxID), int(limit)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: popping stat batch: %w", err)
	}
	out := make([][]byte, 0, len(vals))
	for _, v := range vals {
		out = append(out, []byte(v))
	}
	return out, nil
}

// StatsBufferLen reports how many frames are buffered (used in tests).
func (r *Redis) StatsBufferLen(ctx context.Context, sandboxID string) (int64, error) {
	return r.client.LLen(ctx, statsKey(sandboxID)).Result()
}

// StatFrame is the JSON shape buffered in Redis between the stats collector
// and the Postgres flush worker.
type StatFrame struct {
	SandboxID        string    `json:"sandbox_id"`
	CPUPercent       float32   `json:"cpu_percent"`
	MemoryUsedBytes  int64     `json:"memory_used_bytes"`
	MemoryLimitBytes int64     `json:"memory_limit_bytes"`
	NetworkRXBytes   int64     `json:"network_rx_bytes"`
	NetworkTXBytes   int64     `json:"network_tx_bytes"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// EncodeStatFrame renders a frame for buffering.
func EncodeStatFrame(f StatFrame) ([]byte, error) {
	return json.Marshal(f)
}

// DecodeStatFrame parses a buffered frame.
func DecodeStatFrame(raw []byte) (*StatFrame, error) {
	var f StatFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("cache: decoding buffered stat frame: %w", err)
	}
	return &f, nil
}