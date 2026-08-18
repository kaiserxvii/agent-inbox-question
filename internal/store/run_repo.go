package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

type RunRepo struct {
	db *DB
}

// FinishAttemptParams describes the durable terminal state for one attempt and
// its task. CommentAuthor and CommentBody must either both be set or both empty.
type FinishAttemptParams struct {
	RunID         int64
	TaskID        int64
	RunStatus     domain.RunStatus
	ExitReason    domain.ExitReason
	Output        string
	TokensUsed    int
	Error         string
	TaskStatus    domain.TaskStatus
	CommentAuthor string
	CommentBody   string
}

func NewRunRepo(db *DB) *RunRepo {
	return &RunRepo{db: db}
}

func (r *RunRepo) Create(taskID int64, sessionID string, tokenBudget int) (*domain.Run, error) {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	res, err := r.db.sql.Exec(
		`INSERT INTO runs (task_id, session_id, status, exit_reason, output, tokens_used, token_budget, error, started_at)
		 VALUES (?, ?, ?, '', '', 0, ?, '', ?)`,
		taskID, sessionID, domain.RunRunning, tokenBudget, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get run id: %w", err)
	}
	return &domain.Run{
		ID:          id,
		TaskID:      taskID,
		SessionID:   sessionID,
		Status:      domain.RunRunning,
		TokenBudget: tokenBudget,
		StartedAt:   now,
	}, nil
}

func (r *RunRepo) UpdateProgress(id int64, output string, tokensUsed int) error {
	_, err := r.db.sql.Exec(
		"UPDATE runs SET output = ?, tokens_used = ? WHERE id = ?",
		output, tokensUsed, id,
	)
	if err != nil {
		return fmt.Errorf("update run progress: %w", err)
	}
	return nil
}

// FinishAttempt atomically finalizes a run, transitions its task, and records
// an optional comment.
func (r *RunRepo) FinishAttempt(params FinishAttemptParams) error {
	if err := domain.ValidateTerminalOutcome(params.RunStatus, params.ExitReason, params.TaskStatus); err != nil {
		return fmt.Errorf("validate attempt outcome: %w", err)
	}
	if err := domain.Transition(domain.TaskInProgress, params.TaskStatus); err != nil {
		return fmt.Errorf("validate final task status: %w", err)
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
		params.RunStatus,
		params.ExitReason,
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
		params.TaskStatus,
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

func (r *RunRepo) ListByTask(taskID int64) ([]*domain.Run, error) {
	rows, err := r.db.sql.Query(
		`SELECT id, task_id, session_id, status, exit_reason, output,
		        tokens_used, token_budget, error, started_at, finished_at
		 FROM runs WHERE task_id = ? ORDER BY id`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var runs []*domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (r *RunRepo) CountByTask(taskID int64) (int, error) {
	var count int
	err := r.db.sql.QueryRow("SELECT COUNT(*) FROM runs WHERE task_id = ?", taskID).Scan(&count)
	return count, err
}

func scanRun(rows *sql.Rows) (*domain.Run, error) {
	var run domain.Run
	var status, exitReason, startedAt string
	var finishedAt sql.NullString
	if err := rows.Scan(
		&run.ID, &run.TaskID, &run.SessionID, &status, &exitReason,
		&run.Output, &run.TokensUsed, &run.TokenBudget, &run.Error,
		&startedAt, &finishedAt,
	); err != nil {
		return nil, fmt.Errorf("scan run: %w", err)
	}
	run.Status = domain.RunStatus(status)
	run.ExitReason = domain.ExitReason(exitReason)
	var err error
	run.StartedAt, err = time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return nil, fmt.Errorf("parse started_at: %w", err)
	}
	if finishedAt.Valid {
		t, err := time.Parse(time.RFC3339, finishedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse finished_at: %w", err)
		}
		run.FinishedAt = &t
	}
	return &run, nil
}
