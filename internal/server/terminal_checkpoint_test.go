package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/agent"
	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

func TestServerRecoversTokenExhaustionPublishedBeforeFinalization(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	tasks := store.NewTaskRepo(db)
	attempts := store.NewAttemptRepo(db)
	runs := store.NewRunRepo(db)
	resetInterval := time.Hour
	recoveryAt := time.Now().UTC().Add(-2 * resetInterval).Truncate(time.Second)
	task, err := tasks.Create("two-window task", "[steps:2] [budget:400]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := agent.Start(
		dataDir,
		task.Title,
		task.Description,
		nil,
		agent.WithNoDelay(),
	)
	if err != nil {
		t.Fatalf("Start session: %v", err)
	}
	const leaseDuration = 200 * time.Millisecond
	run, err := attempts.StartOwnedAttempt(store.StartAttemptParams{
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskTodo,
		SessionID:      session.ID(),
		TokenBudget:    session.AttemptAllowance(),
		StartStep:      session.CompletedSteps(),
		StartedAt:      time.Now().UTC(),
		LeaseDuration:  leaseDuration,
	})
	if err != nil {
		t.Fatalf("StartOwnedAttempt: %v", err)
	}
	if err := session.BindAttempt(run.ID, run.OwnerToken); err != nil {
		t.Fatalf("BindAttempt: %v", err)
	}
	initialCheckpoint, err := session.Checkpoint()
	if err != nil {
		t.Fatalf("initial Checkpoint: %v", err)
	}
	if err := attempts.UpdateProgress(store.AttemptProgress{
		RunID:      run.ID,
		OwnerToken: run.OwnerToken,
		Checkpoint: initialCheckpoint,
	}); err != nil {
		t.Fatalf("publish initial checkpoint: %v", err)
	}

	var output []string
	outcome, err := session.RunFenced(
		context.Background(),
		func() error {
			return attempts.RenewLease(run.ID, run.OwnerToken, leaseDuration)
		},
		func(change agent.StateCommit) error {
			if change.Event != nil {
				output = append(output, change.Event.Output)
			}
			return attempts.UpdateProgress(store.AttemptProgress{
				RunID:      run.ID,
				OwnerToken: run.OwnerToken,
				Output:     strings.Join(output, "\n"),
				TokensUsed: change.TokensUsed,
				Checkpoint: change.Checkpoint,
			})
		},
	)
	if err != nil {
		t.Fatalf("RunFenced: %v", err)
	}
	if outcome.Kind != agent.TokenBudgetExhausted {
		t.Fatalf("outcome = %d, want token exhaustion", outcome.Kind)
	}
	if session.CompletedSteps() != 1 {
		t.Fatalf("completed steps = %d, want 1", session.CompletedSteps())
	}

	time.Sleep(2 * leaseDuration)
	clock := newFakeClock(recoveryAt)
	srv, err := New(
		Config{ResetInterval: resetInterval, ScanInterval: 2 * resetInterval},
		Dependencies{
			DataDir:       dataDir,
			Tasks:         tasks,
			Attempts:      attempts,
			Continuations: store.NewContinuationRepo(db),
			Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
			RunnerOptions: runner.Options{NoDelay: true},
		},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.clock = clock

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		clock.Advance(0)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run after cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop after cancellation")
		}
	})

	select {
	case wait := <-clock.waiting:
		if wait != resetInterval {
			t.Errorf("wait after recovery = %s, want reset interval %s", wait, resetInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not recover the expired attempt")
	}

	history, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask after recovery: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("runs after recovery = %d, want only the expired attempt", len(history))
	}
	if history[0].ExitReason != domain.ExitTokenBudgetExhausted {
		t.Errorf(
			"recovered exit reason = %q, want %q",
			history[0].ExitReason,
			domain.ExitTokenBudgetExhausted,
		)
	}
	recovered, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get after recovery: %v", err)
	}
	wantEligible := recoveryAt.Add(resetInterval)
	if eligible := recovered.Continuation.EligibleAt(); eligible == nil || !eligible.Equal(wantEligible) {
		t.Errorf("eligibility = %v, want %s", eligible, wantEligible)
	}
	if recovered.Continuation.Kind() == domain.ContinuationStopped {
		t.Errorf("continuation stopped after recovery: %s", recovered.Continuation.Reason())
	}

	clock.Advance(resetInterval)
	select {
	case <-clock.waiting:
	case <-time.After(time.Second):
		t.Fatal("server did not continue after the provider reset")
	}

	completed, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get after continuation: %v", err)
	}
	if completed.Status != domain.TaskDone {
		t.Errorf("status after reset = %q, want done", completed.Status)
	}
	if completed.Continuation.Kind() == domain.ContinuationStopped {
		t.Errorf("completed task entered stopped state: %s", completed.Continuation.Reason())
	}
	history, err = runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask after continuation: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("runs after continuation = %d, want expired attempt and reset continuation", len(history))
	}
}
