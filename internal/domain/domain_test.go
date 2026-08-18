package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTransition(t *testing.T) {
	allowed := []struct {
		from, to TaskStatus
	}{
		{TaskTodo, TaskInProgress},
		{TaskInProgress, TaskDone},
		{TaskInProgress, TaskFailed},
		{TaskFailed, TaskInProgress},
	}
	for _, tc := range allowed {
		if err := Transition(tc.from, tc.to); err != nil {
			t.Errorf("Transition(%q, %q) should be allowed, got: %v", tc.from, tc.to, err)
		}
	}

	rejected := []struct {
		from, to TaskStatus
	}{
		{TaskTodo, TaskDone},
		{TaskTodo, TaskFailed},
		{TaskTodo, TaskTodo},
		{TaskInProgress, TaskTodo},
		{TaskInProgress, TaskInProgress},
		{TaskDone, TaskDone},
		{TaskDone, TaskTodo},
		{TaskDone, TaskInProgress},
		{TaskDone, TaskFailed},
		{TaskFailed, TaskTodo},
		{TaskFailed, TaskDone},
		{TaskFailed, TaskFailed},
	}
	for _, tc := range rejected {
		err := Transition(tc.from, tc.to)
		if err == nil {
			t.Errorf("Transition(%q, %q) should be rejected, got nil", tc.from, tc.to)
			continue
		}
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("Transition(%q, %q) error should wrap ErrInvalidTransition, got: %v", tc.from, tc.to, err)
		}
	}
}

func TestParseTaskStatus(t *testing.T) {
	for _, s := range []string{"todo", "in_progress", "failed", "done"} {
		st, err := ParseTaskStatus(s)
		if err != nil {
			t.Errorf("ParseTaskStatus(%q) unexpected error: %v", s, err)
		}
		if string(st) != s {
			t.Errorf("ParseTaskStatus(%q) = %q", s, st)
		}
	}
	if _, err := ParseTaskStatus("bogus"); err == nil {
		t.Error("ParseTaskStatus(bogus) should fail")
	}
}

func TestAttemptOutcomeDeterminesEveryTerminalStatus(t *testing.T) {
	tests := []struct {
		outcome    AttemptOutcome
		runStatus  RunStatus
		exitReason ExitReason
		taskStatus TaskStatus
	}{
		{AttemptCompleted, RunSucceeded, ExitCompleted, TaskDone},
		{AttemptAgentError, RunErrored, ExitAgentError, TaskFailed},
		{AttemptTokenExhausted, RunTokenExhausted, ExitTokenBudgetExhausted, TaskFailed},
	}

	for _, test := range tests {
		state, err := test.outcome.TerminalState()
		if err != nil {
			t.Fatalf("TerminalState(%q): %v", test.outcome, err)
		}
		if state.RunStatus != test.runStatus ||
			state.ExitReason != test.exitReason ||
			state.TaskStatus != test.taskStatus {
			t.Errorf(
				"TerminalState(%q) = (%q, %q, %q), want (%q, %q, %q)",
				test.outcome,
				state.RunStatus,
				state.ExitReason,
				state.TaskStatus,
				test.runStatus,
				test.exitReason,
				test.taskStatus,
			)
		}
	}
}

func TestTokenExhaustionStopsRetryWithoutDiscardingProviderReset(t *testing.T) {
	finishedAt := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	completion, err := DecideAttemptCompletion(
		AttemptTokenExhausted,
		finishedAt,
		time.Hour,
		false,
	)
	if err != nil {
		t.Fatalf("DecideAttemptCompletion: %v", err)
	}
	if completion.Outcome() != AttemptTokenExhausted {
		t.Errorf("outcome = %q, want token exhausted", completion.Outcome())
	}
	decision := completion.Continuation()
	if decision.Kind() != ContinuationStopped {
		t.Errorf("continuation = %q, want stopped", decision.Kind())
	}
	wantReset := finishedAt.Add(time.Hour)
	if reset := decision.EligibleAt(); reset == nil || !reset.Equal(wantReset) {
		t.Errorf("provider reset = %v, want %s", reset, wantReset)
	}
	if decision.Reason() != AutoRetryNoProgressReason {
		t.Errorf("reason = %q, want %q", decision.Reason(), AutoRetryNoProgressReason)
	}
}

func TestAttemptCompletionRejectsUnknownOutcome(t *testing.T) {
	if _, err := NewAttemptCompletion(AttemptOutcome("unknown"), NoContinuation()); err == nil {
		t.Fatal("NewAttemptCompletion accepted unknown outcome")
	}
}

func TestAttemptCompletionRejectsIncompatibleContinuation(t *testing.T) {
	resetAt := time.Date(2026, time.August, 17, 13, 0, 0, 0, time.UTC)
	stopped, err := StoppedContinuation(resetAt, AutoRetryNoProgressReason)
	if err != nil {
		t.Fatalf("StoppedContinuation: %v", err)
	}
	if _, err := NewAttemptCompletion(AttemptCompleted, stopped); err == nil {
		t.Fatal("NewAttemptCompletion accepted stopped retry for completed attempt")
	}
}
