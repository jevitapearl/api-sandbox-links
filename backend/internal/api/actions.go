package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/api-sandbox-links/backend/internal/db"
	"github.com/api-sandbox-links/backend/internal/orchestrator"
)

// extendLifetimeRequest is the body of POST /{id}/actions/extend.
type extendLifetimeRequest struct {
	AdditionalSeconds int64 `json:"additional_seconds"`
}

// handleListDeployments returns the sandbox's deploy/build history.
func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListDeployments(sb.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list deployments")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": rows})
}

// handleExtendLifetime pushes expires_at forward by the requested amount but
// never past the hard cap (MaxLifetimeSeconds from original creation), and
// recomputes lifetime_seconds so proportional warnings stay meaningful.
func (s *Server) handleExtendLifetime(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		return
	}
	if sb.DestroyedAt != nil {
		writeErr(w, http.StatusConflict, "this sandbox is already destroyed and cannot be extended")
		return
	}
	var req extendLifetimeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "additional_seconds is required")
		return
	}
	if req.AdditionalSeconds <= 0 {
		writeErr(w, http.StatusBadRequest, "additional_seconds must be positive")
		return
	}

	softCap := sb.CreatedAt.Add(time.Duration(s.maxLifetime()) * time.Second)
	newExpiry := sb.ExpiresAt.Add(time.Duration(req.AdditionalSeconds) * time.Second)
	if newExpiry.After(softCap) {
		newExpiry = softCap
	}
	if !newExpiry.After(sb.ExpiresAt) {
		writeJSON(w, http.StatusOK, map[string]any{
			"sandbox":     s.sandboxJSON(sb),
			"extended_by": int64(0),
			"at_cap":      true,
		})
		return
	}

	if err := s.store.UpdateSandbox(sb.ID, map[string]any{
		"expires_at":       newExpiry,
		"lifetime_seconds": int64(newExpiry.Sub(sb.CreatedAt).Seconds()),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not extend lifetime")
		return
	}
	sb.ExpiresAt = newExpiry
	sb.LifetimeSeconds = int64(newExpiry.Sub(sb.CreatedAt).Seconds())

	s.logger.Info("lifetime extended", slog.String("sandbox", sb.ID),
		slog.Time("expires_at", newExpiry))
	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox":     s.sandboxJSON(sb),
		"extended_by": req.AdditionalSeconds,
		"at_cap":      newExpiry.Equal(softCap),
	})
}

// handleDestroySandbox is the manual, irreversible teardown. It reuses the
// same teardown code as the expiry reaper but records the reason as
// user_requested. Kept distinct from the automatic path in the UI.
func (s *Server) handleDestroySandbox(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		return
	}
	if sb.DestroyedAt != nil {
		writeErr(w, http.StatusConflict, "already destroyed")
		return
	}
	if err := s.destroyPermentally(r.Context(), sb, db.DestroyUserRequested); err != nil {
		s.logger.Warn("manual destroy failed", slog.String("sandbox", sb.ID), slog.String("err", err.Error()))
		writeErr(w, http.StatusInternalServerError, "could not destroy sandbox")
		return
	}
	writeJSON(w, http.StatusOK, s.sandboxJSON(sb))
}

// handleRedeploy triggers a manual rebuild (used after edits are saved and by
// the "redeploy" button). Runs in the background.
func (s *Server) handleRedeploy(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		return
	}
	if sb.DestroyedAt != nil {
		writeErr(w, http.StatusConflict, "destroyed sandboxes cannot be redeployed; recreate instead")
		return
	}
	opts := orchestrator.DeployOptions{Trigger: db.TriggerManual}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.deployer.DeploySandbox(ctx, sb, opts); err != nil {
			s.logger.Warn("manual redeploy failed", slog.String("sandbox", sb.ID), slog.String("err", err.Error()))
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"redeploying": true})
}

// handleSandboxWarnings returns the lifetime-warning state for a sandbox so
// the frontend can render banners on list and detail views (section 2a req 3).
func (s *Server) handleSandboxWarnings(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		return
	}
	thresholds, err := s.store.ListWarningThresholds(sb.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read warnings")
		return
	}
	remaining := time.Until(sb.ExpiresAt)

	warnings := make([]map[string]any, 0, len(thresholds))
	for _, t := range thresholds {
		warnings = append(warnings, map[string]any{
			"threshold": t,
			"title":     warningTitle(t),
			"message":   warningMessage(t, remaining),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id":        sb.ID,
		"expires_at":        sb.ExpiresAt,
		"remaining_seconds": int64(remaining / time.Second),
		"warnings":          warnings,
	})
}

// handleStatsHistory serves persisted resource snapshots for the dashboard.
func (s *Server) handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.requireOwnedSandbox(w, r)
	if !ok {
		return
	}
	since := time.Now().Add(-30 * time.Minute)
	rows, err := s.store.QueryResourceSnapshots(sb.ID, since, time.Now(), 500)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not query resource history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": rows})
}

// destroyPermentally tears a sandbox down and records its tombstone. The typo
// in the name is intentional documentation: this is permanent and unforgettable.
func (s *Server) destroyPermentally(ctx context.Context, sb *db.Sandbox, reason db.DestructionReason) error {
	if sb.ContainerID != "" {
		if err := s.orch.TeardownSandbox(ctx, sb); err != nil {
			return fmt.Errorf("teardown: %w", err)
		}
	}
	now := time.Now()
	return s.store.UpdateSandbox(sb.ID, map[string]any{
		"status":             string(db.StatusDeleted),
		"destroyed_at":       now,
		"destruction_reason": string(reason),
		"container_id":       "",
	})
}