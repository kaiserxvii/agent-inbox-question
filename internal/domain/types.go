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
	RunInterrupted    RunStatus = "interrupted"
)

type ExitReason string

const (
	ExitNone                 ExitReason = ""
	ExitCompleted            ExitReason = "completed"
	ExitAgentError           ExitReason = "agent_error"
	ExitTokenBudgetExhausted ExitReason = "token_budget_exhausted"
	ExitInterrupted          ExitReason = "interrupted"
)

type AttemptOutcome string

const (
	AttemptCompleted      AttemptOutcome = "completed"
	AttemptAgentError     AttemptOutcome = "agent_error"
	AttemptTokenExhausted AttemptOutcome = "token_exhausted"
	AttemptInterrupted    AttemptOutcome = "interrupted"
)

type TerminalAttemptState struct {
	RunStatus  RunStatus
	ExitReason ExitReason
	TaskStatus TaskStatus
}

func (o AttemptOutcome) TerminalState() (TerminalAttemptState, error) {
	switch o {
	case AttemptCompleted:
		return TerminalAttemptState{
			RunStatus:  RunSucceeded,
			ExitReason: ExitCompleted,
			TaskStatus: TaskDone,
		}, nil
	case AttemptAgentError:
		return TerminalAttemptState{
			RunStatus:  RunErrored,
			ExitReason: ExitAgentError,
			TaskStatus: TaskFailed,
		}, nil
	case AttemptTokenExhausted:
		return TerminalAttemptState{
			RunStatus:  RunTokenExhausted,
			ExitReason: ExitTokenBudgetExhausted,
			TaskStatus: TaskFailed,
		}, nil
	case AttemptInterrupted:
		return TerminalAttemptState{
			RunStatus:  RunInterrupted,
			ExitReason: ExitInterrupted,
			TaskStatus: TaskFailed,
		}, nil
	default:
		return TerminalAttemptState{}, fmt.Errorf("unknown attempt outcome: %q", o)
	}
}

type Task struct {
	ID              int64
	Title           string
	Description     string
	Status          TaskStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
	NextEligibleAt  *time.Time
	AutoRetryState  string
	AutoRetryReason string
}

type Run struct {
	ID             int64
	TaskID         int64
	SessionID      string
	Status         RunStatus
	ExitReason     ExitReason
	Output         string
	TokensUsed     int
	TokenBudget    int
	Error          string
	StartedAt      time.Time
	FinishedAt     *time.Time
	OwnerToken     string
	LeaseExpiresAt *time.Time
	StartStep      int
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
	ErrLeaseLost         = errors.New("attempt lease lost")
)

// TaskStatusConflict reports the task status observed when a compare-and-swap
// transition loses a race.
type TaskStatusConflict struct {
	TaskID        int64
	Expected      TaskStatus
	Observed      TaskStatus
	ExpectedRunID int64
	ObservedRunID int64
}

func (e *TaskStatusConflict) Error() string {
	return fmt.Sprintf(
		"%v: task %d expected status %q, observed %q",
		ErrConflict,
		e.TaskID,
		e.Expected,
		e.Observed,
	)
}

func (e *TaskStatusConflict) Unwrap() error {
	return ErrConflict
}

var allowedTransitions = map[TaskStatus]map[TaskStatus]bool{
	TaskTodo:       {TaskInProgress: true},
	TaskInProgress: {TaskDone: true, TaskFailed: true},
	TaskFailed:     {TaskInProgress: true},
}

func Transition(from, to TaskStatus) error {
	if targets, ok := allowedTransitions[from]; ok && targets[to] {
		return nil
	}
	return fmt.Errorf("%w: cannot transition from %q to %q", ErrInvalidTransition, from, to)
}
