package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

// AttemptRepo owns persistence operations that span an attempt, its task, and
// any comment produced by the attempt.
type AttemptRepo struct {
	db *DB
}

// ResumeCandidate is the task state and terminal predecessor observed by one
// database statement.
type ResumeCandidate struct {
	TaskStatus domain.TaskStatus
	RunID      int64
	SessionID  string
	ExitReason domain.ExitReason
}

// FinishAttemptParams describes the attempt data stored alongside its single
// terminal outcome. CommentAuthor and CommentBody must both be set or empty.
type FinishAttemptParams struct {
	RunID         int64
	TaskID        int64
	Outcome       domain.AttemptOutcome
	Output        string
	TokensUsed    int
	Error         string
	CommentAuthor string
	CommentBody   string
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
		        COALESCE(candidate.exit_reason, '')
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
	candidate := &ResumeCandidate{}
	if err := row.Scan(
		&status,
		&candidate.RunID,
		&candidate.SessionID,
		&exitReason,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan resume candidate: %w", err)
	}
	candidate.TaskStatus = domain.TaskStatus(status)
	candidate.ExitReason = domain.ExitReason(exitReason)
	return candidate, nil
}

func (r *AttemptRepo) UpdateProgress(id int64, output string, tokensUsed int) error {
	_, err := r.db.sql.Exec(
		"UPDATE runs SET output = ?, tokens_used = ? WHERE id = ?",
		output,
		tokensUsed,
		id,
	)
	if err != nil {
		return fmt.Errorf("update run progress: %w", err)
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

	now := time.Now().UTC().Format(time.RFC3339)
	runResult, err := tx.Exec(
		`UPDATE runs
		 SET status = ?, exit_reason = ?, output = ?, tokens_used = ?, error = ?, finished_at = ?
		 WHERE id = ? AND task_id = ? AND status = ?`,
		state.RunStatus,
		state.ExitReason,
		params.Output,
		params.TokensUsed,
		params.Error,
		now,
		params.RunID,
		params.TaskID,
		domain.RunRunning,
	)
	if err != nil {
		return rollback(fmt.Errorf("finish run: %w", err))
	}
	rows, err := runResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count finished runs: %w", err))
	}
	if rows != 1 {
		return rollback(fmt.Errorf("%w: run %d is no longer running", domain.ErrConflict, params.RunID))
	}

	taskResult, err := tx.Exec(
		`UPDATE tasks SET status = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		state.TaskStatus,
		now,
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
	if err := domain.Transition(expectedStatus, domain.TaskInProgress); err != nil {
		return nil, err
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

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	result, err := tx.Exec(
		`UPDATE tasks SET status = ?, updated_at = ?
		 WHERE id = ? AND status = ?
		   AND (
		     (? = 0 AND NOT EXISTS (SELECT 1 FROM runs WHERE task_id = ?))
		     OR
		     (? <> 0 AND ? = (SELECT id FROM runs WHERE task_id = ? ORDER BY id DESC LIMIT 1))
		   )`,
		domain.TaskInProgress,
		nowStr,
		taskID,
		expectedStatus,
		expectedRunID,
		taskID,
		expectedRunID,
		expectedRunID,
		taskID,
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
			taskID,
		).Scan(&observed, &observedRunID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rollback(domain.ErrNotFound)
			}
			return rollback(fmt.Errorf("read conflicting task status: %w", err))
		}
		return rollback(&domain.TaskStatusConflict{
			TaskID:        taskID,
			Expected:      expectedStatus,
			Observed:      observed,
			ExpectedRunID: expectedRunID,
			ObservedRunID: observedRunID,
		})
	}

	result, err = tx.Exec(
		`INSERT INTO runs (task_id, session_id, status, exit_reason, output, tokens_used, token_budget, error, started_at)
		 VALUES (?, ?, ?, '', '', 0, ?, '', ?)`,
		taskID,
		sessionID,
		domain.RunRunning,
		tokenBudget,
		nowStr,
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
		ID:          runID,
		TaskID:      taskID,
		SessionID:   sessionID,
		Status:      domain.RunRunning,
		TokenBudget: tokenBudget,
		StartedAt:   now,
	}, nil
}
