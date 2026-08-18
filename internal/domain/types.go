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

// ProviderWindowOrigin distinguishes a fresh provider reset allowance from a
// continuation that inherited the remainder of an existing window. Unknown is
// retained for checkpoints written before this fact was persisted.
type ProviderWindowOrigin string

const (
	ProviderWindowUnknown   ProviderWindowOrigin = ""
	ProviderWindowFresh     ProviderWindowOrigin = "fresh"
	ProviderWindowContinued ProviderWindowOrigin = "continued"
)

type ContinuationKind string

const (
	ContinuationNone      ContinuationKind = ""
	ContinuationScheduled ContinuationKind = "scheduled"
	ContinuationStopped   ContinuationKind = "stopped"

	AutoRetryNoProgressReason = "auto-retry stopped: next step requires more than the configured window"
)

// ContinuationDecision keeps provider reset eligibility independent from the
// automatic retry decision while preventing invalid persisted combinations.
type ContinuationDecision struct {
	kind       ContinuationKind
	eligibleAt *time.Time
	reason     string
}

func NoContinuation() ContinuationDecision {
	return ContinuationDecision{}
}

func ScheduledContinuation(eligibleAt time.Time) ContinuationDecision {
	eligibleAt = eligibleAt.UTC()
	return ContinuationDecision{kind: ContinuationScheduled, eligibleAt: &eligibleAt}
}

func StoppedContinuation(eligibleAt time.Time, reason string) (ContinuationDecision, error) {
	if reason == "" {
		return ContinuationDecision{}, errors.New("stopped continuation requires a reason")
	}
	eligibleAt = eligibleAt.UTC()
	return ContinuationDecision{
		kind:       ContinuationStopped,
		eligibleAt: &eligibleAt,
		reason:     reason,
	}, nil
}

func ParseContinuation(
	kind string,
	eligibleAt *time.Time,
	reason string,
) (ContinuationDecision, error) {
	switch ContinuationKind(kind) {
	case ContinuationNone:
		if eligibleAt != nil || reason != "" {
			return ContinuationDecision{}, errors.New("empty continuation has scheduling data")
		}
		return NoContinuation(), nil
	case ContinuationScheduled:
		if eligibleAt == nil || reason != "" {
			return ContinuationDecision{}, errors.New("scheduled continuation requires only eligibility")
		}
		return ScheduledContinuation(*eligibleAt), nil
	case ContinuationStopped:
		if eligibleAt == nil {
			return ContinuationDecision{}, errors.New("stopped continuation requires provider reset eligibility")
		}
		return StoppedContinuation(*eligibleAt, reason)
	default:
		return ContinuationDecision{}, fmt.Errorf("unknown continuation state: %q", kind)
	}
}

func (d ContinuationDecision) Kind() ContinuationKind { return d.kind }
func (d ContinuationDecision) Reason() string         { return d.reason }

func (d ContinuationDecision) EligibleAt() *time.Time {
	if d.eligibleAt == nil {
		return nil
	}
	eligibleAt := *d.eligibleAt
	return &eligibleAt
}

type AttemptCompletion struct {
	outcome      AttemptOutcome
	continuation ContinuationDecision
}

func NewAttemptCompletion(
	outcome AttemptOutcome,
	continuation ContinuationDecision,
) (AttemptCompletion, error) {
	if _, err := outcome.TerminalState(); err != nil {
		return AttemptCompletion{}, err
	}
	kind := continuation.Kind()
	valid := false
	switch outcome {
	case AttemptCompleted, AttemptAgentError:
		valid = kind == ContinuationNone
	case AttemptTokenExhausted:
		valid = kind == ContinuationNone ||
			kind == ContinuationScheduled ||
			kind == ContinuationStopped
	case AttemptInterrupted:
		valid = kind == ContinuationScheduled
	}
	if !valid {
		return AttemptCompletion{}, fmt.Errorf(
			"attempt outcome %q cannot use continuation %q",
			outcome,
			kind,
		)
	}
	return AttemptCompletion{outcome: outcome, continuation: continuation}, nil
}

type AttemptCompletionInput struct {
	Outcome       AttemptOutcome
	FinishedAt    time.Time
	ResetInterval time.Duration
	Progressed    bool
	WindowOrigin  ProviderWindowOrigin
}

func DecideAttemptCompletion(input AttemptCompletionInput) (AttemptCompletion, error) {
	switch input.WindowOrigin {
	case ProviderWindowUnknown, ProviderWindowFresh, ProviderWindowContinued:
	default:
		return AttemptCompletion{}, fmt.Errorf(
			"unknown provider window origin: %q",
			input.WindowOrigin,
		)
	}
	decision := NoContinuation()
	switch input.Outcome {
	case AttemptTokenExhausted:
		if input.ResetInterval > 0 {
			resetAt := input.FinishedAt.UTC().Add(input.ResetInterval)
			if input.Progressed || input.WindowOrigin != ProviderWindowFresh {
				decision = ScheduledContinuation(resetAt)
			} else {
				var err error
				decision, err = StoppedContinuation(resetAt, AutoRetryNoProgressReason)
				if err != nil {
					return AttemptCompletion{}, err
				}
			}
		}
	case AttemptInterrupted:
		decision = ScheduledContinuation(input.FinishedAt)
	case AttemptCompleted, AttemptAgentError:
	default:
		return AttemptCompletion{}, fmt.Errorf("unknown attempt outcome: %q", input.Outcome)
	}
	return NewAttemptCompletion(input.Outcome, decision)
}

func (c AttemptCompletion) Outcome() AttemptOutcome {
	return c.outcome
}

func (c AttemptCompletion) Continuation() ContinuationDecision {
	return c.continuation
}

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
	ID           int64
	Title        string
	Description  string
	Status       TaskStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Continuation ContinuationDecision
}

type Run struct {
	ID                int64
	TaskID            int64
	SessionID         string
	Status            RunStatus
	ExitReason        ExitReason
	Output            string
	TokensUsed        int
	TokenBudget       int
	Error             string
	StartedAt         time.Time
	FinishedAt        *time.Time
	OwnerToken        string
	LeaseExpiresAt    *time.Time
	StartStep         int
	SessionCheckpoint string
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
	ErrNotEligible       = errors.New("attempt is not eligible")
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
