package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

type RunRepo struct {
	db *DB
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

func (r *RunRepo) Finish(id int64, status domain.RunStatus, exitReason domain.ExitReason, output string, tokensUsed int, errText string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.sql.Exec(
		"UPDATE runs SET status = ?, exit_reason = ?, output = ?, tokens_used = ?, error = ?, finished_at = ? WHERE id = ?",
		status, exitReason, output, tokensUsed, errText, now, id,
	)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

func (r *RunRepo) ListByTask(taskID int64) ([]*domain.Run, error) {
	rows, err := r.db.sql.Query(
		`SELECT id, task_id, session_id, status, exit_reason, output, tokens_used, token_budget, error, started_at, finished_at
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
