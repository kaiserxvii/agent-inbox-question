package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/agent"
	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiting chan time.Duration
	wake    chan struct{}
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{
		now:     now,
		waiting: make(chan time.Duration, 10),
		wake:    make(chan struct{}, 10),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Wait(ctx context.Context, duration time.Duration) error {
	c.waiting <- duration
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.wake:
		return nil
	}
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
	c.wake <- struct{}{}
}

func TestServerWaitsUntilEligibilityThenContinuesExhaustedTask(t *testing.T) {
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
	continuations := store.NewContinuationRepo(db)
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	resetInterval := time.Hour

	task, err := tasks.Create("two-window task", "[steps:2] [budget:400]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	deps := runner.Deps{
		DataDir:  dataDir,
		Tasks:    tasks,
		Attempts: attempts,
		Options: runner.Options{
			NoDelay:       true,
			Now:           func() time.Time { return now },
			ResetInterval: resetInterval,
		},
	}
	if err := runner.Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	clock := newFakeClock(now)
	srv, err := New(
		Config{
			ResetInterval: resetInterval,
			ScanInterval:  2 * resetInterval,
		},
		Dependencies{
			DataDir:       dataDir,
			Tasks:         tasks,
			Attempts:      attempts,
			Continuations: continuations,
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

	select {
	case duration := <-clock.waiting:
		if duration != resetInterval {
			t.Errorf("first wait = %s, want %s", duration, resetInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not wait for task eligibility")
	}

	before, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get before eligibility: %v", err)
	}
	if before.Status != domain.TaskFailed {
		t.Errorf("status before eligibility = %q, want failed", before.Status)
	}
	if count, err := runs.CountByTask(task.ID); err != nil {
		t.Fatalf("CountByTask before eligibility: %v", err)
	} else if count != 1 {
		t.Errorf("runs before eligibility = %d, want 1", count)
	}

	clock.Advance(resetInterval)
	select {
	case <-clock.waiting:
	case <-time.After(time.Second):
		t.Fatal("server did not finish continuation")
	}

	after, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get after eligibility: %v", err)
	}
	if after.Status != domain.TaskDone {
		t.Errorf("status after eligibility = %q, want done", after.Status)
	}
	if count, err := runs.CountByTask(task.ID); err != nil {
		t.Fatalf("CountByTask after eligibility: %v", err)
	} else if count != 2 {
		t.Errorf("runs after eligibility = %d, want 2", count)
	}

	cancel()
	clock.Advance(0)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestServerContinuesAcrossRepeatedBudgetResetsUntilCompletion(t *testing.T) {
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
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	resetInterval := time.Hour
	task, err := tasks.Create("many-window task", "[steps:6] [budget:400]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := runner.Execute(context.Background(), runner.Deps{
		DataDir:  dataDir,
		Tasks:    tasks,
		Attempts: attempts,
		Options: runner.Options{
			NoDelay:       true,
			Now:           func() time.Time { return now },
			ResetInterval: resetInterval,
		},
	}, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	clock := newFakeClock(now)
	srv, err := New(
		Config{
			ResetInterval: resetInterval,
			ScanInterval:  2 * resetInterval,
		},
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

	for range 10 {
		select {
		case <-clock.waiting:
		case <-time.After(time.Second):
			t.Fatal("server did not publish its next wait")
		}
		current, err := tasks.Get(task.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if current.Status == domain.TaskDone {
			break
		}
		clock.Advance(resetInterval)
	}

	completed, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get completed task: %v", err)
	}
	if completed.Status != domain.TaskDone {
		t.Fatalf("status = %q, want done", completed.Status)
	}
	if count, err := runs.CountByTask(task.ID); err != nil {
		t.Fatalf("CountByTask: %v", err)
	} else if count < 3 {
		t.Errorf("runs = %d, want at least 3 reset windows", count)
	}

	cancel()
	clock.Advance(0)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}

func TestServerDoesNotRetryFreshWindowThatCompletesNoSteps(t *testing.T) {
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
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	task, err := tasks.Create("oversized next step", "[steps:1] [budget:1]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := runner.Execute(context.Background(), runner.Deps{
		DataDir:  dataDir,
		Tasks:    tasks,
		Attempts: attempts,
		Options: runner.Options{
			NoDelay:       true,
			Now:           func() time.Time { return now },
			ResetInterval: time.Hour,
		},
	}, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	stopped, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stopped.Continuation.Kind() != domain.ContinuationStopped {
		t.Errorf("auto retry state = %q, want stopped", stopped.Continuation.Kind())
	}
	const wantReason = "auto-retry stopped: next step requires more than the configured window"
	if stopped.Continuation.Reason() != wantReason {
		t.Errorf("auto retry reason = %q, want %q", stopped.Continuation.Reason(), wantReason)
	}
	wantReset := now.Add(time.Hour)
	if stopped.Continuation.EligibleAt() == nil || !stopped.Continuation.EligibleAt().Equal(wantReset) {
		t.Errorf("next eligible at = %v, want provider reset %s", stopped.Continuation.EligibleAt(), wantReset)
	}

	clock := newFakeClock(now.Add(24 * time.Hour))
	srv, err := New(
		Config{ResetInterval: time.Hour, ScanInterval: time.Minute},
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
	select {
	case <-clock.waiting:
	case <-time.After(time.Second):
		t.Fatal("server did not finish its scan")
	}
	if count, err := runs.CountByTask(task.ID); err != nil {
		t.Fatalf("CountByTask: %v", err)
	} else if count != 1 {
		t.Errorf("runs = %d, want 1", count)
	}

	cancel()
	clock.Advance(0)
	<-done
}

func TestServerDoesNotRetryAgentErrors(t *testing.T) {
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
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	task, err := tasks.Create("agent failure", "[steps:3] [fail-at:1] [budget:5000]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := runner.Execute(context.Background(), runner.Deps{
		DataDir:  dataDir,
		Tasks:    tasks,
		Attempts: attempts,
		Options: runner.Options{
			NoDelay:       true,
			Now:           func() time.Time { return now },
			ResetInterval: time.Hour,
		},
	}, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	failed, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if failed.Continuation.Kind() != "" || failed.Continuation.EligibleAt() != nil {
		t.Fatalf(
			"agent error scheduling = (%q, %v), want no automatic retry",
			failed.Continuation.Kind(),
			failed.Continuation.EligibleAt(),
		)
	}

	clock := newFakeClock(now.Add(24 * time.Hour))
	srv, err := New(
		Config{ResetInterval: time.Hour, ScanInterval: time.Minute},
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
	select {
	case <-clock.waiting:
	case <-time.After(time.Second):
		t.Fatal("server did not finish its scan")
	}
	if count, err := runs.CountByTask(task.ID); err != nil {
		t.Fatalf("CountByTask: %v", err)
	} else if count != 1 {
		t.Errorf("runs = %d, want 1", count)
	}

	cancel()
	clock.Advance(0)
	<-done
}

func TestServerRecoversExpiredRunFromSessionCheckpoint(t *testing.T) {
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
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	task, err := tasks.Create("crashed task", "[steps:3] [budget:5000]")
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
	run, err := attempts.StartOwnedAttempt(store.StartAttemptParams{
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskTodo,
		SessionID:      session.ID(),
		TokenBudget:    session.AttemptAllowance(),
		StartStep:      session.CompletedSteps(),
		StartedAt:      now.Add(-2 * time.Minute),
		LeaseDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("StartOwnedAttempt: %v", err)
	}
	if err := session.BindAttempt(run.ID, run.OwnerToken); err != nil {
		t.Fatalf("BindAttempt: %v", err)
	}

	crashCtx, crash := context.WithCancel(context.Background())
	completedSteps := 0
	outcome, err := session.Run(crashCtx, func(event agent.Event) {
		if event.StepName == "summary" {
			return
		}
		completedSteps++
		crash()
	})
	if err != nil {
		t.Fatalf("Run session to crash checkpoint: %v", err)
	}
	if outcome.Kind != agent.Interrupted || completedSteps != 1 {
		t.Fatalf("checkpoint outcome = %d with %d steps, want interrupted with 1", outcome.Kind, completedSteps)
	}
	remaining := session.RemainingBudget()

	clock := newFakeClock(now)
	srv, err := New(
		Config{ResetInterval: time.Hour, ScanInterval: time.Minute},
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
	select {
	case <-clock.waiting:
	case <-time.After(time.Second):
		t.Fatal("server did not finish crash recovery")
	}

	recovered, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get recovered task: %v", err)
	}
	if recovered.Status != domain.TaskDone {
		t.Errorf("recovered task status = %q, want done", recovered.Status)
	}
	history, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("runs = %d, want expired and recovery runs", len(history))
	}
	if history[0].Status != domain.RunInterrupted || history[0].Output == "" {
		t.Errorf(
			"expired run = (status %q, output %q), want interrupted with reconciled output",
			history[0].Status,
			history[0].Output,
		)
	}
	if history[1].TokenBudget != remaining {
		t.Errorf("recovery allowance = %d, want remaining %d", history[1].TokenBudget, remaining)
	}

	cancel()
	clock.Advance(0)
	<-done
}

func TestServerSchedulesPreviouslyUnscheduledTokenExhaustion(t *testing.T) {
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
	task, err := tasks.Create("legacy exhaustion", "[steps:2] [budget:400]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := runner.Execute(context.Background(), runner.Deps{
		DataDir:  dataDir,
		Tasks:    tasks,
		Attempts: attempts,
		Options:  runner.Options{NoDelay: true},
	}, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	initialRun, err := runs.LatestByTask(task.ID)
	if err != nil {
		t.Fatalf("LatestByTask: %v", err)
	}
	if initialRun.FinishedAt == nil {
		t.Fatal("initial run has no finish timestamp")
	}

	resetInterval := time.Hour
	clock := newFakeClock(*initialRun.FinishedAt)
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
	select {
	case wait := <-clock.waiting:
		if wait != resetInterval {
			t.Errorf("wait = %s, want reset interval %s", wait, resetInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not schedule legacy exhaustion")
	}

	scheduled, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get scheduled task: %v", err)
	}
	wantEligible := initialRun.FinishedAt.Add(resetInterval)
	if scheduled.Continuation.EligibleAt() == nil || !scheduled.Continuation.EligibleAt().Equal(wantEligible) {
		t.Errorf("next eligibility = %v, want %s", scheduled.Continuation.EligibleAt(), wantEligible)
	}

	cancel()
	clock.Advance(0)
	<-done
}
