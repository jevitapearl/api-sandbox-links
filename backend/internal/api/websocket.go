package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/api-sandbox-links/backend/internal/orchestrator"
	"nhooyr.io/websocket"
)

// logFrame is one line sent to the frontend's xterm panel. stream tells the UI
// whether it came from stdout or stderr so it can color differently.
type logFrame struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// wsEvent is the envelope used by both log and stats sockets: a kind plus a
// typed payload, kept flexible so handlers can add status events later.
type wsEvent struct {
	Kind   string        `json:"kind"` // "log" | "stats" | "status"
	Line   string        `json:"line,omitempty"`
	Stream string        `json:"stream,omitempty"`
	Stats  *statsPayload `json:"stats,omitempty"`
	Status string        `json:"status,omitempty"`
}

type statsPayload struct {
	CPUPct           float64   `json:"cpu_pct"`
	MemoryUsedBytes  int64     `json:"memory_used_bytes"`
	MemoryLimitBytes int64     `json:"memory_limit_bytes"`
	NetworkRXBytes   int64     `json:"network_rx_bytes"`
	NetworkTXBytes   int64     `json:"network_tx_bytes"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// handleLogsWS streams a sandbox container's docker logs to the browser over a
// WebSocket. The connection owns a context that is cancelled when the client
// disconnects, so no goroutine outlives its socket (BUILD_PROMPT §6: every
// background goroutine is cancellable).
func (s *Server) handleLogsWS(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.closeOnClientDisconnect(conn, ctx, cancel)

	if sb.ContainerID == "" || sb.DestroyedAt != nil {
		_ = s.sendEvent(conn, wsEvent{Kind: "status", Status: "no-container"})
		return
	}

	reader, err := s.orch.DockerLogs(ctx, sb.ContainerID)
	if err != nil {
		_ = s.sendEvent(conn, wsEvent{Kind: "status", Status: "log-error"})
		return
	}
	defer reader.Close()

	if err := s.streamLogFrames(ctx, conn, reader); err != nil && ctx.Err() == nil {
		s.logger.Debug("logs ws ended", slog.String("sandbox", sb.ID), slog.String("err", err.Error()))
	}
}

// streamLogFrames demultiplexes Docker's multiplexed log stream (8-byte header
// per frame: 1 stream byte, 3 reserved, 4 big-endian size) and emits each line
// as a wsEvent. Unicode-safe: lines are split on '\n' and partial buffers kept.
func (s *Server) streamLogFrames(ctx context.Context, conn *websocket.Conn, reader io.Reader) error {
	var (
		header = make([]byte, 8)
		line   = make([]byte, 0, 1024)
		stream = "stdout"
	)
	flush := func() {
		if len(line) == 0 {
			return
		}
		_ = s.sendEvent(conn, wsEvent{Kind: "log", Stream: stream, Line: string(line)})
		line = line[:0]
	}

	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			return err // EOF (container restarted or stopped) or stream error
		}
		if header[0] == 1 {
			stream = "stderr"
		} else {
			stream = "stdout"
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size > 1<<20 { // 1 MB guard against corrupt frames
			continue
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return err
		}
		for _, b := range chunk {
			if b == '\n' {
				flush()
				continue
			}
			if len(line) >= 1024*1024 {
				flush() // a single pathological long line becomes many frames
			}
			line = append(line, b)
		}
	}
}

// handleStatsWS streams live CPU/memory frames from the sandbox's stats
// collector over a WebSocket, refreshing roughly every two seconds.
func (s *Server) handleStatsWS(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.closeOnClientDisconnect(conn, ctx, cancel)

	if sb.ContainerID == "" || sb.DestroyedAt != nil {
		_ = s.sendEvent(conn, wsEvent{Kind: "status", Status: "no-container"})
		return
	}

	collector, err := s.orch.EnsureStatsCollector(sb.ID, sb.ContainerID)
	if err != nil {
		_ = s.sendEvent(conn, wsEvent{Kind: "status", Status: "stats-error"})
		return
	}
	sub := collector.Subscribe()
	defer collector.Unsubscribe(sub)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case f := <-sub:
			if err := s.sendStatsEvent(conn, f); err != nil {
				return
			}
		case <-ticker.C:
			if last := collector.Last(); last != nil {
				if err := s.sendStatsEvent(conn, *last); err != nil {
					return
				}
			}
		}
	}
}

func (s *Server) sendStatsEvent(conn *websocket.Conn, f orchestrator.StatsFrame) error {
	return s.sendEvent(conn, wsEvent{
		Kind: "stats",
		Stats: &statsPayload{
			CPUPct:           f.CPUPercent,
			MemoryUsedBytes:  f.MemoryUsedBytes,
			MemoryLimitBytes: f.MemoryLimitBytes,
			NetworkRXBytes:   f.NetworkRXBytes,
			NetworkTXBytes:   f.NetworkTXBytes,
			RecordedAt:       f.RecordedAt,
		},
	})
}

// requireOwnedSandbox is resolved from the signed session cookie; it lives in
// sandboxes.go (shared by all handlers). WebSocket handshakes carry cookies
// like any other request, so the same helper applies.

// closeOnClientDisconnect watches the socket and cancels the stream context the
// moment the client goes away.
func (s *Server) closeOnClientDisconnect(conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc) {
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()
}

// sendEvent pushes a JSON event to the socket with a short write timeout.
func (s *Server) sendEvent(conn *websocket.Conn, ev wsEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}