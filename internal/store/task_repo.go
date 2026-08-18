package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

type TaskRepo struct {
	db *DB
}

func NewTaskRepo(db *DB) *TaskRepo {
	return &TaskRepo{db: db}
}

func (r *TaskRepo) Create(title, description string) (*domain.Task, error) {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	res, err := r.db.sql.Exec(
		"INSERT INTO tasks (title, description, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		title, description, domain.TaskTodo, nowStr, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get task id: %w", err)
	}
	return &domain.Task{
		ID:          id,
		Title:       title,
		Description: description,
		Status:      domain.TaskTodo,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (r *TaskRepo) Get(id int64) (*domain.Task, error) {
	row := r.db.sql.QueryRow(
		`SELECT id, title, description, status, created_at, updated_at,
		        next_eligible_at, auto_retry_state, auto_retry_reason
		 FROM tasks WHERE id = ?`,
		id,
	)
	return scanTask(row)
}

func (r *TaskRepo) List(statusFilter *domain.TaskStatus) ([]*domain.Task, error) {
	var rows *sql.Rows
	var err error
	if statusFilter != nil {
		rows, err = r.db.sql.Query(
			`SELECT id, title, description, status, created_at, updated_at,
			        next_eligible_at, auto_retry_state, auto_retry_reason
			 FROM tasks WHERE status = ? ORDER BY id`,
			*statusFilter,
		)
	} else {
		rows, err = r.db.sql.Query(
			`SELECT id, title, description, status, created_at, updated_at,
			        next_eligible_at, auto_retry_state, auto_retry_reason FROM tasks
			 ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END, id`,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*domain.Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (r *TaskRepo) Transition(id int64, from, to domain.TaskStatus) error {
	if err := domain.Transition(from, to); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := r.db.sql.Exec(
		"UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = ?",
		to, now, id, from,
	)
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: task %d is no longer in status %q", domain.ErrConflict, id, from)
	}
	return nil
}

func (r *TaskRepo) CountByStatus() (map[domain.TaskStatus]int, error) {
	rows, err := r.db.sql.Query("SELECT status, COUNT(*) FROM tasks GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("count by status: %w", err)
	}
	defer rows.Close()

	counts := make(map[domain.TaskStatus]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan count: %w", err)
		}
		counts[domain.TaskStatus(status)] = count
	}
	return counts, rows.Err()
}

func (r *TaskRepo) OldestByStatus(status domain.TaskStatus) (*domain.Task, error) {
	row := r.db.sql.QueryRow(
		`SELECT id, title, description, status, created_at, updated_at,
		        next_eligible_at, auto_retry_state, auto_retry_reason
		 FROM tasks WHERE status = ? ORDER BY id LIMIT 1`,
		status,
	)
	t, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	return t, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTaskFrom(s scanner) (*domain.Task, error) {
	var t domain.Task
	var status string
	var createdAt, updatedAt string
	var nextEligibleAt sql.NullString
	if err := s.Scan(
		&t.ID,
		&t.Title,
		&t.Description,
		&status,
		&createdAt,
		&updatedAt,
		&nextEligibleAt,
		&t.AutoRetryState,
		&t.AutoRetryReason,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Status = domain.TaskStatus(status)
	var err error
	t.CreatedAt, err = time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	t.UpdatedAt, err = time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	if nextEligibleAt.Valid {
		next, parseErr := time.Parse(time.RFC3339Nano, nextEligibleAt.String)
		if parseErr != nil {
			return nil, fmt.Errorf("parse next_eligible_at: %w", parseErr)
		}
		t.NextEligibleAt = &next
	}
	return &t, nil
}

func scanTask(row *sql.Row) (*domain.Task, error) {
	return scanTaskFrom(row)
}

func scanTaskRows(rows *sql.Rows) (*domain.Task, error) {
	return scanTaskFrom(rows)
}
