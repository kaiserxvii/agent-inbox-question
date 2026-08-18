package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

type part1FailureSeed struct {
	dataDir    string
	taskID     int64
	finishedAt time.Time
}

func seedPart1Failure(
	t *testing.T,
	title string,
	description string,
	wantOutcome agent.OutcomeKind,
) part1FailureSeed {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "sessions"), 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}
	session, err := agent.Start(dataDir, title, description, nil, agent.WithNoDelay())
	if err != nil {
		t.Fatalf("Start legacy session: %v", err)
	}
	var output []string
	outcome, err := session.Run(context.Background(), func(event agent.Event) {
		output = append(output, event.Output)
	})
	if err != nil {
		t.Fatalf("Run legacy session: %v", err)
	}
	if outcome.Kind != wantOutcome {
		t.Fatalf("legacy outcome = %d, want %d", outcome.Kind, wantOutcome)
	}
	sessionPath := filepath.Join(dataDir, "sessions", session.ID()+".json")
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("read legacy session: %v", err)
	}
	var legacyState map[string]any
	if err := json.Unmarshal(data, &legacyState); err != nil {
		t.Fatalf("parse legacy session: %v", err)
	}
	delete(legacyState, "attempt_run_id")
	delete(legacyState, "attempt_owner_token")
	delete(legacyState, "attempt_start_step")
	delete(legacyState, "attempt_halt_reason")
	delete(legacyState, "attempt_window_origin")
	legacyState["budget_model_version"] = 2
	data, err = json.MarshalIndent(legacyState, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy session: %v", err)
	}
	if err := os.WriteFile(sessionPath, data, 0o644); err != nil {
		t.Fatalf("write legacy session: %v", err)
	}

	dbPath := filepath.Join(dataDir, "inbox.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'todo',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL REFERENCES tasks(id),
			session_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'running',
			exit_reason TEXT NOT NULL DEFAULT '',
			output TEXT NOT NULL DEFAULT '',
			tokens_used INTEGER NOT NULL DEFAULT 0,
			token_budget INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT
		);
		CREATE TABLE comments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id INTEGER NOT NULL REFERENCES tasks(id),
			author TEXT NOT NULL,
			body TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX idx_runs_task_id ON runs(task_id);
		CREATE INDEX idx_comments_task_id ON comments(task_id);
		INSERT INTO schema_migrations(version, applied_at)
		VALUES (1, datetime('now')), (2, datetime('now')), (3, datetime('now'));
	`); err != nil {
		t.Fatalf("create version-3 database: %v", err)
	}
	finishedAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	result, err := raw.Exec(
		`INSERT INTO tasks(title, description, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		title,
		description,
		domain.TaskFailed,
		finishedAt.Add(-time.Minute).Format(time.RFC3339),
		finishedAt.Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert legacy task: %v", err)
	}
	taskID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("legacy task ID: %v", err)
	}
	runStatus := domain.RunErrored
	exitReason := domain.ExitAgentError
	errorText := "legacy agent error"
	if wantOutcome == agent.TokenBudgetExhausted {
		runStatus = domain.RunTokenExhausted
		exitReason = domain.ExitTokenBudgetExhausted
		errorText = ""
	}
	if _, err := raw.Exec(
		`INSERT INTO runs(
		   task_id, session_id, status, exit_reason, output, tokens_used,
		   token_budget, error, started_at, finished_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID,
		session.ID(),
		runStatus,
		exitReason,
		strings.Join(output, "\n"),
		session.TokensUsed(),
		session.AttemptAllowance(),
		errorText,
		finishedAt.Add(-time.Minute).Format(time.RFC3339),
		finishedAt.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
	return part1FailureSeed{dataDir: dataDir, taskID: taskID, finishedAt: finishedAt}
}

func TestVersionThreeFailuresRemainResumableAfterUpgrade(t *testing.T) {
	t.Run("manual resume", func(t *testing.T) {
		seed := seedPart1Failure(
			t,
			"legacy manual failure",
			"[steps:1] [fail-at:1] [budget:5000]",
			agent.Errored,
		)
		db, err := store.Open(seed.dataDir)
		if err != nil {
			t.Fatalf("Open upgraded database: %v", err)
		}
		defer db.Close()
		tasks := store.NewTaskRepo(db)
		attempts := store.NewAttemptRepo(db)
		runs := store.NewRunRepo(db)
		if _, err := runner.Resume(context.Background(), runner.Deps{
			DataDir:  seed.dataDir,
			Tasks:    tasks,
			Attempts: attempts,
			Options:  runner.Options{NoDelay: true},
		}, seed.taskID); err != nil {
			t.Fatalf("Resume upgraded failure: %v", err)
		}
		task, err := tasks.Get(seed.taskID)
		if err != nil {
			t.Fatalf("Get resumed task: %v", err)
		}
		if task.Status != domain.TaskDone {
			t.Errorf("resumed status = %q, want done", task.Status)
		}
		history, err := runs.ListByTask(seed.taskID)
		if err != nil {
			t.Fatalf("ListByTask: %v", err)
		}
		if len(history) != 2 || history[1].SessionCheckpoint == "" {
			t.Errorf("upgraded history = %#v, want checkpointed resumed run", history)
		}
	})

	t.Run("server continuation", func(t *testing.T) {
		seed := seedPart1Failure(
			t,
			"two-window task",
			"[steps:2] [budget:400]",
			agent.TokenBudgetExhausted,
		)
		db, err := store.Open(seed.dataDir)
		if err != nil {
			t.Fatalf("Open upgraded database: %v", err)
		}
		defer db.Close()
		tasks := store.NewTaskRepo(db)
		attempts := store.NewAttemptRepo(db)
		runs := store.NewRunRepo(db)
		resetInterval := time.Hour
		clock := newFakeClock(seed.finishedAt.Add(resetInterval))
		srv, err := New(
			Config{ResetInterval: resetInterval, ScanInterval: time.Minute},
			Dependencies{
				DataDir:       seed.dataDir,
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
		go func() { done <- srv.Run(ctx) }()
		select {
		case <-clock.waiting:
		case <-time.After(time.Second):
			t.Fatal("server did not finish upgraded continuation")
		}
		task, err := tasks.Get(seed.taskID)
		if err != nil {
			t.Fatalf("Get continued task: %v", err)
		}
		if task.Status != domain.TaskDone {
			t.Errorf("continued status = %q, want done", task.Status)
		}
		history, err := runs.ListByTask(seed.taskID)
		if err != nil {
			t.Fatalf("ListByTask: %v", err)
		}
		if len(history) != 2 || history[1].SessionCheckpoint == "" {
			t.Errorf("upgraded history = %#v, want checkpointed continuation", history)
		}
		cancel()
		clock.Advance(0)
		if err := <-done; err != nil {
			t.Errorf("Run after cancellation: %v", err)
		}
	})
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
