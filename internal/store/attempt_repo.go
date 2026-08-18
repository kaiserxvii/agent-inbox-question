package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

const defaultAttemptLease = 30 * time.Second

// AttemptRepo owns persistence operations that span an attempt, its task, and
// any comment produced by the attempt.
type AttemptRepo struct {
	db *DB
}

// ResumeCandidate is the task state and terminal predecessor observed by one
// database statement.
type ResumeCandidate struct {
	TaskStatus      domain.TaskStatus
	RunID           int64
	SessionID       string
	ExitReason      domain.ExitReason
	NextEligibleAt  *time.Time
	AutoRetryState  string
	AutoRetryReason string
}

// FinishAttemptParams describes the attempt data stored alongside its single
// terminal outcome. CommentAuthor and CommentBody must both be set or empty.
type FinishAttemptParams struct {
	RunID           int64
	TaskID          int64
	OwnerToken      string
	Outcome         domain.AttemptOutcome
	Output          string
	TokensUsed      int
	Error           string
	CommentAuthor   string
	CommentBody     string
	FinishedAt      time.Time
	LeaseCheckedAt  time.Time
	NextEligibleAt  *time.Time
	AutoRetryState  string
	AutoRetryReason string
	RecoverExpired  bool
}

type StartAttemptParams struct {
	TaskID         int64
	ExpectedStatus domain.TaskStatus
	ExpectedRunID  int64
	SessionID      string
	TokenBudget    int
	StartStep      int
	StartedAt      time.Time
	LeaseDuration  time.Duration
}

type AttemptProgress struct {
	RunID      int64
	OwnerToken string
	ObservedAt time.Time
	Output     string
	TokensUsed int
}

func NewAttemptRepo(db *DB) *AttemptRepo {
	return &AttemptRepo{db: db}
}

// GetResumeCandidate snapshots task status and its latest terminal run so a
// caller cannot accidentally adopt a concurrent invocation's running attempt.
func (r *AttemptRepo) GetResumeCandidate(taskID int64) (*ResumeCandidate, error) {
	row := r.db.sql.QueryRow(
		`SELECT tasks.status,
		        COALESCE(candidate.id, 0),
		        COALESCE(candidate.session_id, ''),
		        COALESCE(candidate.exit_reason, ''),
		        tasks.next_eligible_at,
		        tasks.auto_retry_state,
		        tasks.auto_retry_reason
		 FROM tasks
		 LEFT JOIN runs AS candidate ON candidate.id = (
		   SELECT id
		   FROM runs
		   WHERE task_id = tasks.id AND status <> ?
		   ORDER BY id DESC
		   LIMIT 1
		 )
		 WHERE tasks.id = ?`,
		domain.RunRunning,
		taskID,
	)
	var status string
	var exitReason string
	var nextEligibleAt sql.NullString
	candidate := &ResumeCandidate{}
	if err := row.Scan(
		&status,
		&candidate.RunID,
		&candidate.SessionID,
		&exitReason,
		&nextEligibleAt,
		&candidate.AutoRetryState,
		&candidate.AutoRetryReason,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan resume candidate: %w", err)
	}
	candidate.TaskStatus = domain.TaskStatus(status)
	candidate.ExitReason = domain.ExitReason(exitReason)
	if nextEligibleAt.Valid {
		next, err := time.Parse(time.RFC3339Nano, nextEligibleAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse resume eligibility: %w", err)
		}
		candidate.NextEligibleAt = &next
	}
	return candidate, nil
}

func (r *AttemptRepo) UpdateProgress(progress AttemptProgress) error {
	result, err := r.db.sql.Exec(
		`UPDATE runs SET output = ?, tokens_used = ?
		 WHERE id = ? AND status = ? AND owner_token = ? AND lease_expires_at > ?`,
		progress.Output,
		progress.TokensUsed,
		progress.RunID,
		domain.RunRunning,
		progress.OwnerToken,
		progress.ObservedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("update run progress: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated run progress: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: run %d cannot publish progress", domain.ErrLeaseLost, progress.RunID)
	}
	return nil
}

func (r *AttemptRepo) RenewLease(
	id int64,
	ownerToken string,
	now time.Time,
	leaseDuration time.Duration,
) error {
	result, err := r.db.sql.Exec(
		`UPDATE runs SET lease_expires_at = ?
		 WHERE id = ? AND status = ? AND owner_token = ? AND lease_expires_at > ?`,
		now.UTC().Add(leaseDuration).Format(time.RFC3339Nano),
		id,
		domain.RunRunning,
		ownerToken,
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("renew attempt lease: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count renewed attempt lease: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: run %d cannot renew lease", domain.ErrLeaseLost, id)
	}
	return nil
}

// FinishAttempt atomically finalizes a run, transitions its task, and records
// an optional comment.
func (r *AttemptRepo) FinishAttempt(params FinishAttemptParams) error {
	state, err := params.Outcome.TerminalState()
	if err != nil {
		return fmt.Errorf("validate attempt outcome: %w", err)
	}
	commentIncomplete := (params.CommentAuthor == "") != (params.CommentBody == "")
	if commentIncomplete {
		return errors.New("attempt comment requires both author and body")
	}

	tx, err := r.db.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin finish attempt transaction: %w", err)
	}
	rollback := func(cause error) error {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(cause, fmt.Errorf("rollback finish attempt transaction: %w", rollbackErr))
		}
		return cause
	}

	finishedAt := params.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}
	now := finishedAt.Format(time.RFC3339Nano)
	leaseCheckedAt := params.LeaseCheckedAt
	if leaseCheckedAt.IsZero() {
		leaseCheckedAt = time.Now().UTC()
	}
	var nextEligibleAt any
	if params.NextEligibleAt != nil {
		nextEligibleAt = params.NextEligibleAt.UTC().Format(time.RFC3339Nano)
	}
	runResult, err := tx.Exec(
		`UPDATE runs
		 SET status = ?, exit_reason = ?, output = ?, tokens_used = ?, error = ?, finished_at = ?
		 WHERE id = ? AND task_id = ? AND status = ?
		   AND (
		     (? = 0 AND owner_token = ? AND lease_expires_at > ?)
		     OR
		     (? = 1 AND lease_expires_at <= ?)
		   )`,
		state.RunStatus,
		state.ExitReason,
		params.Output,
		params.TokensUsed,
		params.Error,
		now,
		params.RunID,
		params.TaskID,
		domain.RunRunning,
		boolInt(params.RecoverExpired),
		params.OwnerToken,
		leaseCheckedAt.Format(time.RFC3339Nano),
		boolInt(params.RecoverExpired),
		leaseCheckedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return rollback(fmt.Errorf("finish run: %w", err))
	}
	rows, err := runResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count finished runs: %w", err))
	}
	if rows != 1 {
		return rollback(fmt.Errorf("%w: run %d cannot be finalized", domain.ErrLeaseLost, params.RunID))
	}

	taskResult, err := tx.Exec(
		`UPDATE tasks
		 SET status = ?, updated_at = ?, next_eligible_at = ?,
		     auto_retry_state = ?, auto_retry_reason = ?
		 WHERE id = ? AND status = ?`,
		state.TaskStatus,
		now,
		nextEligibleAt,
		params.AutoRetryState,
		params.AutoRetryReason,
		params.TaskID,
		domain.TaskInProgress,
	)
	if err != nil {
		return rollback(fmt.Errorf("finish task: %w", err))
	}
	rows, err = taskResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count finished tasks: %w", err))
	}
	if rows != 1 {
		return rollback(fmt.Errorf("%w: task %d is no longer in progress", domain.ErrConflict, params.TaskID))
	}

	if params.CommentAuthor != "" {
		_, err = tx.Exec(
			`INSERT INTO comments (task_id, author, body, created_at) VALUES (?, ?, ?, ?)`,
			params.TaskID,
			params.CommentAuthor,
			params.CommentBody,
			now,
		)
		if err != nil {
			return rollback(fmt.Errorf("create attempt comment: %w", err))
		}
	}

	if err := tx.Commit(); err != nil {
		return rollback(fmt.Errorf("commit finish attempt transaction: %w", err))
	}
	return nil
}

func (r *AttemptRepo) NextExpired(now time.Time) (*domain.Run, error) {
	row := r.db.sql.QueryRow(
		`SELECT id, task_id, session_id, status, exit_reason, output,
		        tokens_used, token_budget, error, started_at, finished_at,
		        owner_token, lease_expires_at, start_step
		 FROM runs
		 WHERE status = ? AND lease_expires_at <= ?
		 ORDER BY lease_expires_at, id
		 LIMIT 1`,
		domain.RunRunning,
		now.UTC().Format(time.RFC3339Nano),
	)
	return scanRunFrom(row)
}

// StartAttempt atomically claims a task and records the running attempt that
// owns the claim. An expectedRunID of zero requires no prior attempts;
// otherwise it must still be the task's latest run.
func (r *AttemptRepo) StartAttempt(
	taskID int64,
	expectedStatus domain.TaskStatus,
	expectedRunID int64,
	sessionID string,
	tokenBudget int,
) (*domain.Run, error) {
	now := time.Now().UTC()
	return r.StartOwnedAttempt(StartAttemptParams{
		TaskID:         taskID,
		ExpectedStatus: expectedStatus,
		ExpectedRunID:  expectedRunID,
		SessionID:      sessionID,
		TokenBudget:    tokenBudget,
		StartedAt:      now,
		LeaseDuration:  defaultAttemptLease,
	})
}

func (r *AttemptRepo) StartOwnedAttempt(params StartAttemptParams) (*domain.Run, error) {
	if err := domain.Transition(params.ExpectedStatus, domain.TaskInProgress); err != nil {
		return nil, err
	}
	if params.LeaseDuration <= 0 {
		return nil, errors.New("attempt lease duration must be positive")
	}
	ownerToken, err := generateOwnerToken()
	if err != nil {
		return nil, fmt.Errorf("generate attempt owner token: %w", err)
	}

	tx, err := r.db.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin start attempt transaction: %w", err)
	}
	rollback := func(cause error) (*domain.Run, error) {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return nil, errors.Join(cause, fmt.Errorf("rollback start attempt transaction: %w", rollbackErr))
		}
		return nil, cause
	}

	now := params.StartedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowStr := now.Format(time.RFC3339Nano)
	leaseExpiresAt := now.Add(params.LeaseDuration)
	result, err := tx.Exec(
		`UPDATE tasks
		 SET status = ?, updated_at = ?, next_eligible_at = NULL,
		     auto_retry_state = '', auto_retry_reason = ''
		 WHERE id = ? AND status = ?
		   AND (
		     (? = 0 AND NOT EXISTS (SELECT 1 FROM runs WHERE task_id = ?))
		     OR
		     (? <> 0 AND ? = (SELECT id FROM runs WHERE task_id = ? ORDER BY id DESC LIMIT 1))
		   )`,
		domain.TaskInProgress,
		nowStr,
		params.TaskID,
		params.ExpectedStatus,
		params.ExpectedRunID,
		params.TaskID,
		params.ExpectedRunID,
		params.ExpectedRunID,
		params.TaskID,
	)
	if err != nil {
		return rollback(fmt.Errorf("claim task: %w", err))
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count claimed tasks: %w", err))
	}
	if rows != 1 {
		var observed domain.TaskStatus
		var observedRunID int64
		if err := tx.QueryRow(
			`SELECT status, COALESCE((SELECT MAX(id) FROM runs WHERE task_id = tasks.id), 0)
			 FROM tasks WHERE id = ?`,
			params.TaskID,
		).Scan(&observed, &observedRunID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rollback(domain.ErrNotFound)
			}
			return rollback(fmt.Errorf("read conflicting task status: %w", err))
		}
		return rollback(&domain.TaskStatusConflict{
			TaskID:        params.TaskID,
			Expected:      params.ExpectedStatus,
			Observed:      observed,
			ExpectedRunID: params.ExpectedRunID,
			ObservedRunID: observedRunID,
		})
	}

	result, err = tx.Exec(
		`INSERT INTO runs (
		   task_id, session_id, status, exit_reason, output, tokens_used,
		   token_budget, error, started_at, owner_token, lease_expires_at, start_step
		 ) VALUES (?, ?, ?, '', '', 0, ?, '', ?, ?, ?, ?)`,
		params.TaskID,
		params.SessionID,
		domain.RunRunning,
		params.TokenBudget,
		nowStr,
		ownerToken,
		leaseExpiresAt.Format(time.RFC3339Nano),
		params.StartStep,
	)
	if err != nil {
		return rollback(fmt.Errorf("insert run: %w", err))
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return rollback(fmt.Errorf("get run id: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return rollback(fmt.Errorf("commit start attempt transaction: %w", err))
	}
	return &domain.Run{
		ID:             runID,
		TaskID:         params.TaskID,
		SessionID:      params.SessionID,
		Status:         domain.RunRunning,
		TokenBudget:    params.TokenBudget,
		StartedAt:      now,
		OwnerToken:     ownerToken,
		LeaseExpiresAt: &leaseExpiresAt,
		StartStep:      params.StartStep,
	}, nil
}

func generateOwnerToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
