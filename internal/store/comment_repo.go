package store

import (
	"fmt"
	"time"

	"github.com/villagelabsco/agent-inbox/internal/domain"
)

type CommentRepo struct {
	db *DB
}

func NewCommentRepo(db *DB) *CommentRepo {
	return &CommentRepo{db: db}
}

func (r *CommentRepo) Create(taskID int64, author, body string) (*domain.Comment, error) {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	res, err := r.db.sql.Exec(
		"INSERT INTO comments (task_id, author, body, created_at) VALUES (?, ?, ?, ?)",
		taskID, author, body, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("insert comment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get comment id: %w", err)
	}
	return &domain.Comment{
		ID:        id,
		TaskID:    taskID,
		Author:    author,
		Body:      body,
		CreatedAt: now,
	}, nil
}

func (r *CommentRepo) ListByTask(taskID int64) ([]*domain.Comment, error) {
	rows, err := r.db.sql.Query(
		"SELECT id, task_id, author, body, created_at FROM comments WHERE task_id = ? ORDER BY id",
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("query comments: %w", err)
	}
	defer rows.Close()

	var comments []*domain.Comment
	for rows.Next() {
		var c domain.Comment
		var createdAt string
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Body, &createdAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		var parseErr error
		c.CreatedAt, parseErr = time.Parse(time.RFC3339, createdAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse comment created_at: %w", parseErr)
		}
		comments = append(comments, &c)
	}
	return comments, rows.Err()
}
