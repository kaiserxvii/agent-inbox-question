package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

type Continuation struct {
	TaskID     int64
	EligibleAt time.Time
}

type RunningAttempt struct {
	TaskID         int64
	TaskTitle      string
	RunID          int64
	LeaseExpiresAt time.Time
}

type StoppedContinuation struct {
	TaskID int64
	Reason string
}

type InitializedContinuation struct {
	TaskID       int64
	Continuation domain.ContinuationDecision
}

type ContinuationRepo struct {
	db *DB
}

func NewContinuationRepo(db *DB) *ContinuationRepo {
	return &ContinuationRepo{db: db}
}

func (r *ContinuationRepo) Next() (*Continuation, error) {
	row := r.db.sql.QueryRow(
		`SELECT id, next_eligible_at
		 FROM tasks
		 WHERE status = ? AND auto_retry_state = ? AND next_eligible_at IS NOT NULL
		 ORDER BY next_eligible_at, id
		 LIMIT 1`,
		domain.TaskFailed,
		domain.ContinuationScheduled,
	)
	var continuation Continuation
	var eligibleAt string
	if err := row.Scan(&continuation.TaskID, &eligibleAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan next continuation: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, eligibleAt)
	if err != nil {
		return nil, fmt.Errorf("parse continuation eligibility: %w", err)
	}
	continuation.EligibleAt = parsed
	return &continuation, nil
}

func (r *ContinuationRepo) Current() (*RunningAttempt, error) {
	row := r.db.sql.QueryRow(
		`SELECT tasks.id, tasks.title, runs.id, runs.lease_expires_at
		 FROM runs
		 JOIN tasks ON tasks.id = runs.task_id
		 WHERE runs.status = ?
		 ORDER BY runs.id
		 LIMIT 1`,
		domain.RunRunning,
	)
	var current RunningAttempt
	var leaseExpiresAt string
	if err := row.Scan(
		&current.TaskID,
		&current.TaskTitle,
		&current.RunID,
		&leaseExpiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan current attempt: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, leaseExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse current lease expiry: %w", err)
	}
	current.LeaseExpiresAt = parsed
	return &current, nil
}

func (r *ContinuationRepo) Stopped() ([]StoppedContinuation, error) {
	rows, err := r.db.sql.Query(
		`SELECT id, auto_retry_reason
		 FROM tasks
		 WHERE status = ? AND auto_retry_state = ?
		 ORDER BY id`,
		domain.TaskFailed,
		domain.ContinuationStopped,
	)
	if err != nil {
		return nil, fmt.Errorf("query stopped continuations: %w", err)
	}
	defer rows.Close()

	stopped := []StoppedContinuation{}
	for rows.Next() {
		var item StoppedContinuation
		if err := rows.Scan(&item.TaskID, &item.Reason); err != nil {
			return nil, fmt.Errorf("scan stopped continuation: %w", err)
		}
		stopped = append(stopped, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stopped continuations: %w", err)
	}
	return stopped, nil
}

func (r *ContinuationRepo) InitializeUnscheduled(
	resetInterval time.Duration,
) (*InitializedContinuation, error) {
	row := r.db.sql.QueryRow(
		`SELECT tasks.id, runs.finished_at, runs.tokens_used
		 FROM tasks
		 JOIN runs ON runs.id = (
		   SELECT id FROM runs AS latest
		   WHERE latest.task_id = tasks.id
		   ORDER BY id DESC LIMIT 1
		 )
		 WHERE tasks.status = ?
		   AND tasks.auto_retry_state = ''
		   AND tasks.next_eligible_at IS NULL
		   AND runs.exit_reason = ?
		   AND runs.finished_at IS NOT NULL
		 ORDER BY tasks.id
		 LIMIT 1`,
		domain.TaskFailed,
		domain.ExitTokenBudgetExhausted,
	)
	var taskID int64
	var finishedAt string
	var tokensUsed int
	if err := row.Scan(&taskID, &finishedAt, &tokensUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan unscheduled continuation: %w", err)
	}
	finished, err := time.Parse(time.RFC3339Nano, finishedAt)
	if err != nil {
		return nil, fmt.Errorf("parse unscheduled run finish: %w", err)
	}

	completion, err := domain.DecideAttemptCompletion(
		domain.AttemptTokenExhausted,
		finished,
		resetInterval,
		tokensUsed > 0,
	)
	if err != nil {
		return nil, fmt.Errorf("decide unscheduled continuation: %w", err)
	}
	decision := completion.Continuation()
	initialized := &InitializedContinuation{TaskID: taskID, Continuation: decision}
	var eligibleValue any
	if eligibleAt := decision.EligibleAt(); eligibleAt != nil {
		eligibleValue = eligibleAt.Format(time.RFC3339Nano)
	}
	result, err := r.db.sql.Exec(
		`UPDATE tasks
		 SET next_eligible_at = ?, auto_retry_state = ?, auto_retry_reason = ?
		 WHERE id = ? AND status = ? AND auto_retry_state = '' AND next_eligible_at IS NULL`,
		eligibleValue,
		decision.Kind(),
		decision.Reason(),
		taskID,
		domain.TaskFailed,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize continuation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("count initialized continuations: %w", err)
	}
	if rows != 1 {
		return nil, domain.ErrConflict
	}
	return initialized, nil
}
