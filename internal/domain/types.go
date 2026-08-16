package domain

import (
	"errors"
	"fmt"
	"time"
)

type TaskStatus string

const (
	TaskTodo       TaskStatus = "todo"
	TaskInProgress TaskStatus = "in_progress"
	TaskFailed     TaskStatus = "failed"
	TaskDone       TaskStatus = "done"
)

func ParseTaskStatus(s string) (TaskStatus, error) {
	switch TaskStatus(s) {
	case TaskTodo, TaskInProgress, TaskFailed, TaskDone:
		return TaskStatus(s), nil
	default:
		return "", fmt.Errorf("unknown task status: %q", s)
	}
}

type RunStatus string

const (
	RunRunning        RunStatus = "running"
	RunSucceeded      RunStatus = "succeeded"
	RunErrored        RunStatus = "errored"
	RunTokenExhausted RunStatus = "token_exhausted"
)

type ExitReason string

const (
	ExitNone                 ExitReason = ""
	ExitCompleted            ExitReason = "completed"
	ExitAgentError           ExitReason = "agent_error"
	ExitTokenBudgetExhausted ExitReason = "token_budget_exhausted"
)

type Task struct {
	ID          int64
	Title       string
	Description string
	Status      TaskStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Run struct {
	ID          int64
	TaskID      int64
	SessionID   string
	Status      RunStatus
	ExitReason  ExitReason
	Output      string
	TokensUsed  int
	TokenBudget int
	Error       string
	StartedAt   time.Time
	FinishedAt  *time.Time
}

type Comment struct {
	ID        int64
	TaskID    int64
	Author    string
	Body      string
	CreatedAt time.Time
}

var (
	ErrInvalidTransition = errors.New("invalid status transition")
	ErrConflict          = errors.New("concurrent modification conflict")
	ErrNotFound          = errors.New("not found")
)

var allowedTransitions = map[TaskStatus]map[TaskStatus]bool{
	TaskTodo:       {TaskInProgress: true},
	TaskInProgress: {TaskDone: true, TaskFailed: true},
	TaskDone:       {TaskInProgress: true},
}

func Transition(from, to TaskStatus) error {
	if targets, ok := allowedTransitions[from]; ok && targets[to] {
		return nil
	}
	return fmt.Errorf("%w: cannot transition from %q to %q", ErrInvalidTransition, from, to)
}
