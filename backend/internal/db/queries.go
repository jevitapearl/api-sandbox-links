package db

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Store bundles the gorm handle plus every query. Keeping all SQL-shaped
// operations here (rather than scattered in handlers) makes the access layer
// reviewable and testable in one place.
type Store struct {
	db *gorm.DB
}

// NewStore returns a Store wrapping the given gorm handle.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

func newID() string {
	return uuid.NewString()
}

// ---- users --------------------------------------------------------------

// UpsertUser inserts the user if new, or updates profile fields and the
// (rotated) GitHub token if the user already exists. GitHub identity is the
// unique key, per the schema.
func (s *Store) UpsertUser(u *User) error {
	if u.ID == "" {
		u.ID = newID()
	}
	existing := &User{}
	err := s.db.Where("github_id = ?", u.GithubID).First(existing).Error
	if err == gorm.ErrRecordNotFound {
		return s.db.Create(u).Error
	}
	if err != nil {
		return fmt.Errorf("db: looking up user: %w", err)
	}
	// Preserve the stable UUID; refresh everything else.
	u.ID = existing.ID
	u.CreatedAt = existing.CreatedAt
	return s.db.Save(u).Error
}

// GetUserByGithubID fetches a user by their GitHub numeric identity.
func (s *Store) GetUserByGithubID(id int64) (*User, error) {
	u := &User{}
	if err := s.db.Where("github_id = ?", id).First(u).Error; err != nil {
		return nil, fmt.Errorf("db: fetching user %d: %w", id, err)
	}
	return u, nil
}

// GetUser fetches a user by internal UUID.
func (s *Store) GetUser(id string) (*User, error) {
	u := &User{}
	if err := s.db.First(u, "id = ?", id).Error; err != nil {
		return nil, fmt.Errorf("db: fetching user %s: %w", id, err)
	}
	return u, nil
}

// ---- repositories -------------------------------------------------------

// FindOrCreateRepository returns the user's repository row for githubURL,
// creating it on first use.
func (s *Store) FindOrCreateRepository(userID, githubURL, defaultBranch string) (*Repository, error) {
	repo := &Repository{}
	err := s.db.Where("user_id = ? AND github_url = ?", userID, githubURL).First(repo).Error
	if err == nil {
		return repo, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, fmt.Errorf("db: looking up repository: %w", err)
	}
	repo = &Repository{
		ID:            newID(),
		UserID:        userID,
		GithubURL:     githubURL,
		DefaultBranch: defaultBranch,
	}
	if err := s.db.Create(repo).Error; err != nil {
		return nil, fmt.Errorf("db: creating repository: %w", err)
	}
	return repo, nil
}

// GetRepository fetches a repository by ID.
func (s *Store) GetRepository(id string) (*Repository, error) {
	repo := &Repository{}
	if err := s.db.First(repo, "id = ?", id).Error; err != nil {
		return nil, fmt.Errorf("db: fetching repository %s: %w", id, err)
	}
	return repo, nil
}

// ---- sandboxes ----------------------------------------------------------

// CreateSandbox persists a new sandbox row sized from the deploy request.
func (s *Store) CreateSandbox(sb *Sandbox) error {
	if sb.ID == "" {
		sb.ID = newID()
	}
	sb.ExpiresAt = sb.CreatedAt.Add(time.Duration(sb.LifetimeSeconds) * time.Second)
	return s.db.Create(sb).Error
}

// GetSandbox fetches a sandbox by ID.
func (s *Store) GetSandbox(id string) (*Sandbox, error) {
	sb := &Sandbox{}
	if err := s.db.First(sb, "id = ?", id).Error; err != nil {
		return nil, fmt.Errorf("db: fetching sandbox %s: %w", id, err)
	}
	return sb, nil
}

// GetSandboxBySubdomain fetches a sandbox by its subdomain (used by the
// wake/reverse-proxy gateway to resolve incoming requests).
func (s *Store) GetSandboxBySubdomain(sub string) (*Sandbox, error) {
	sb := &Sandbox{}
	if err := s.db.First(sb, "subdomain = ?", sub).Error; err != nil {
		return nil, fmt.Errorf("db: fetching sandbox by subdomain %s: %w", sub, err)
	}
	return sb, nil
}

// ListSandboxes returns the user's sandboxes, newest first, together with the
// repo URL so the UI can show "this sandbox was forked from X" and recreate.
func (s *Store) ListSandboxes(userID string) ([]*Sandbox, error) {
	var out []*Sandbox
	// Only sandboxes reachable through the user's own repositories are shown.
	sub := s.db.Where("repository_id IN (SELECT id FROM repositories WHERE user_id = ?)", userID)
	if err := sub.Order("created_at DESC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: listing sandboxes: %w", err)
	}
	return out, nil
}

// ListWangableSandboxes lists sandboxes that are still alive (not yet
// destroyed) and not in a terminal failed state for the warnings worker.
func (s *Store) ListActiveSandboxes() ([]*Sandbox, error) {
	var out []*Sandbox
	if err := s.db.Where("destroyed_at IS NULL AND status NOT IN ?",
		[]SandboxStatus{StatusFailed, StatusExpired}).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: listing active sandboxes: %w", err)
	}
	return out, nil
}

// ListDuedExpiredSandboxes finds non-destroyed sandboxes whose deadline has
// passed. This is the only query the destroyer worker makes, so it stays on
// the partial index from the schema.
func (s *Store) ListDuedExpiredSandboxes(now time.Time) ([]*Sandbox, error) {
	var out []*Sandbox
	if err := s.db.Where("expires_at < ? AND destroyed_at IS NULL", now).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: listing expired sandboxes: %w", err)
	}
	return out, nil
}

// UpdateSandbox applies the non-zero fields of patch to the sandbox row.
func (s *Store) UpdateSandbox(id string, patch map[string]any) error {
	patch["updated_at"] = time.Now()
	if err := s.db.Model(&Sandbox{}).Where("id = ?", id).Updates(patch).Error; err != nil {
		return fmt.Errorf("db: updating sandbox %s: %w", id, err)
	}
	return nil
}

// OwnsSandbox reports whether userID owns the sandbox with the given ID.
func (s *Store) OwnsSandbox(sandboxID, userID string) (bool, error) {
	var count int64
	err := s.db.Model(&Sandbox{}).
		Joins("JOIN repositories ON repositories.id = sandboxes.repository_id").
		Where("sandboxes.id = ? AND repositories.user_id = ?", sandboxID, userID).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("db: ownership check: %w", err)
	}
	return count > 0, nil
}

// ---- deployments --------------------------------------------------------

// CreateDeployment records a build attempt in the history table and returns
// the row with its generated ID.
func (s *Store) CreateDeployment(d *Deployment) (*Deployment, error) {
	if d.ID == "" {
		d.ID = newID()
	}
	if err := s.db.Create(d).Error; err != nil {
		return nil, fmt.Errorf("db: creating deployment: %w", err)
	}
	return d, nil
}

// UpdateDeployment patches a deployment row (commonly status and log excerpt).
func (s *Store) UpdateDeployment(id string, patch map[string]any) error {
	if err := s.db.Model(&Deployment{}).Where("id = ?", id).Updates(patch).Error; err != nil {
		return fmt.Errorf("db: updating deployment %s: %w", id, err)
	}
	return nil
}

// ListDeployments returns deploy history for a sandbox, newest first.
func (s *Store) ListDeployments(sandboxID string) ([]*Deployment, error) {
	var out []*Deployment
	if err := s.db.Where("sandbox_id = ?", sandboxID).Order("started_at DESC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: listing deployments: %w", err)
	}
	return out, nil
}

// ---- lifetime warnings --------------------------------------------------

// HasWarningSent reports whether the given threshold warning was already sent.
func (s *Store) HasWarningSent(sandboxID, threshold string) (bool, error) {
	var count int64
	err := s.db.Model(&LifetimeWarning{}).
		Where("sandbox_id = ? AND threshold = ?", sandboxID, threshold).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("db: checking warning: %w", err)
	}
	return count > 0, nil
}

// MarkWarningSent records a threshold warning as delivered.
func (s *Store) MarkWarningSent(sandboxID, threshold string) error {
	w := &LifetimeWarning{ID: newID(), SandboxID: sandboxID, Threshold: threshold}
	return s.db.Create(w).Error
}

// ListWarningThresholds returns the thresholds already sent for a sandbox.
func (s *Store) ListWarningThresholds(sandboxID string) ([]string, error) {
	var out []string
	if err := s.db.Model(&LifetimeWarning{}).
		Where("sandbox_id = ?", sandboxID).
		Pluck("threshold", &out).Error; err != nil {
		return nil, fmt.Errorf("db: listing warnings: %w", err)
	}
	return out, nil
}

// ---- env vars -----------------------------------------------------------

// UpsertEnvVar inserts or replaces one secret env var for a sandbox (values
// stored encrypted; encrypt before calling).
func (s *Store) UpsertEnvVar(sandboxID, key string, value []byte, aiSuggested bool) error {
	v := &EnvVar{}
	err := s.db.Where("sandbox_id = ? AND key = ?", sandboxID, key).First(v).Error
	if err == gorm.ErrRecordNotFound {
		v = &EnvVar{ID: newID(), SandboxID: sandboxID, Key: key, Value: value, IsAISuggested: aiSuggested}
		if err := s.db.Create(v).Error; err != nil {
			return fmt.Errorf("db: creating env var: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: looking up env var: %w", err)
	}
	return s.db.Model(v).Where("id = ?", v.ID).
		Updates(map[string]any{"value": value, "is_ai_suggested": aiSuggested}).Error
}

// ListEnvVars returns all env vars for a sandbox (encrypted values).
func (s *Store) ListEnvVars(sandboxID string) ([]*EnvVar, error) {
	var out []*EnvVar
	if err := s.db.Where("sandbox_id = ?", sandboxID).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: listing env vars: %w", err)
	}
	return out, nil
}

// ---- sidecar databases --------------------------------------------------

// CreateDatabase records a provisioned sidecar database.
func (s *Store) CreateDatabase(d *SandboxDatabase) error {
	if d.ID == "" {
		d.ID = newID()
	}
	return s.db.Create(d).Error
}

// ListSandboxDatabases returns the sidecar DBs attached to a sandbox.
func (s *Store) ListSandboxDatabases(sandboxID string) ([]*SandboxDatabase, error) {
	var out []*SandboxDatabase
	if err := s.db.Where("sandbox_id = ?", sandboxID).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: listing sandbox databases: %w", err)
	}
	return out, nil
}

// GetSandboxDatabase fetches one sidecar DB by id.
func (s *Store) GetSandboxDatabase(id string) (*SandboxDatabase, error) {
	d := &SandboxDatabase{}
	if err := s.db.First(d, "id = ?", id).Error; err != nil {
		return nil, fmt.Errorf("db: fetching sandbox database %s: %w", id, err)
	}
	return d, nil
}

// ---- resource snapshots -------------------------------------------------

// InsertResourceSnapshots bulk-inserts buffered resource samples.
func (s *Store) InsertResourceSnapshots(rows []*ResourceSnapshot) error {
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.CreateInBatches(rows, 200).Error; err != nil {
		return fmt.Errorf("db: inserting resource snapshots: %w", err)
	}
	return nil
}

// QueryResourceSnapshots returns samples within a window, oldest first.
func (s *Store) QueryResourceSnapshots(sandboxID string, since, until time.Time, limit int) ([]*ResourceSnapshot, error) {
	var out []*ResourceSnapshot
	q := s.db.Where("sandbox_id = ? AND recorded_at BETWEEN ? AND ?", sandboxID, since, until)
	if err := q.Order("recorded_at ASC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("db: querying resource snapshots: %w", err)
	}
	return out, nil
}

// ---- file edits ---------------------------------------------------------

// RecordFileEdit logs that a user edited a file in a sandbox.
func (s *Store) RecordFileEdit(sandboxID, path, userID string) error {
	e := &FileEdit{ID: newID(), SandboxID: sandboxID, FilePath: path, EditedBy: userID}
	return s.db.Create(e).Error
}

// MarkFileEditsPushed sets pushed_to_github for a sandbox's pending edits.
func (s *Store) MarkFileEditsPushed(sandboxID string) error {
	return s.db.Model(&FileEdit{}).
		Where("sandbox_id = ? AND pushed_to_github = ?", sandboxID, false).
		Update("pushed_to_github", true).Error
}