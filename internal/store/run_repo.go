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

func NewRunRepo(db *DB) *RunRepo {
	return &RunRepo{db: db}
}

func (r *RunRepo) ListByTask(taskID int64) ([]*domain.Run, error) {
	rows, err := r.db.sql.Query(
		`SELECT id, task_id, session_id, status, exit_reason, output,
		        tokens_used, token_budget, error, started_at, finished_at,
		        owner_token, lease_expires_at, start_step
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

func (r *RunRepo) LatestByTask(taskID int64) (*domain.Run, error) {
	row := r.db.sql.QueryRow(
		`SELECT id, task_id, session_id, status, exit_reason, output,
		        tokens_used, token_budget, error, started_at, finished_at,
		        owner_token, lease_expires_at, start_step
		 FROM runs WHERE task_id = ? ORDER BY id DESC LIMIT 1`,
		taskID,
	)
	return scanRunFrom(row)
}

func (r *RunRepo) CountByTask(taskID int64) (int, error) {
	var count int
	err := r.db.sql.QueryRow("SELECT COUNT(*) FROM runs WHERE task_id = ?", taskID).Scan(&count)
	return count, err
}

func scanRun(rows *sql.Rows) (*domain.Run, error) {
	return scanRunFrom(rows)
}

func scanRunFrom(s scanner) (*domain.Run, error) {
	var run domain.Run
	var status, exitReason, startedAt string
	var finishedAt sql.NullString
	var leaseExpiresAt sql.NullString
	if err := s.Scan(
		&run.ID, &run.TaskID, &run.SessionID, &status, &exitReason,
		&run.Output, &run.TokensUsed, &run.TokenBudget, &run.Error,
		&startedAt, &finishedAt, &run.OwnerToken, &leaseExpiresAt, &run.StartStep,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
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
	if leaseExpiresAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, leaseExpiresAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse lease_expires_at: %w", err)
		}
		run.LeaseExpiresAt = &t
	}
	return &run, nil
}
