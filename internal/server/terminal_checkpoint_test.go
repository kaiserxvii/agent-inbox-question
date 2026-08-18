package server

import (
	"context"
	"errors"
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

const terminalCheckpointLease = 200 * time.Millisecond

var errCrashBeforeTerminalCommit = errors.New("crash before terminal commit")

type terminalCheckpointFixture struct {
	dataDir       string
	db            *store.DB
	tasks         *store.TaskRepo
	attempts      *store.AttemptRepo
	runs          *store.RunRepo
	task          *domain.Task
	session       *agent.Session
	run           *domain.Run
	resetInterval time.Duration
	haltedAt      time.Time
	recoveryAt    time.Time
}

func newTerminalCheckpointFixture(
	t *testing.T,
	downtime time.Duration,
) *terminalCheckpointFixture {
	t.Helper()
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

	recoveryAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	fixture := &terminalCheckpointFixture{
		dataDir:       dataDir,
		db:            db,
		tasks:         store.NewTaskRepo(db),
		attempts:      store.NewAttemptRepo(db),
		runs:          store.NewRunRepo(db),
		resetInterval: time.Hour,
		haltedAt:      recoveryAt.Add(-downtime),
		recoveryAt:    recoveryAt,
	}
	fixture.task, err = fixture.tasks.Create("two-window task", "[steps:2] [budget:400]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fixture.session, err = agent.Start(
		dataDir,
		fixture.task.Title,
		fixture.task.Description,
		nil,
		agent.WithNoDelay(),
		agent.WithNow(func() time.Time { return fixture.haltedAt }),
	)
	if err != nil {
		t.Fatalf("Start session: %v", err)
	}
	fixture.run, err = fixture.attempts.StartOwnedAttempt(store.StartAttemptParams{
		TaskID:         fixture.task.ID,
		ExpectedStatus: domain.TaskTodo,
		SessionID:      fixture.session.ID(),
		TokenBudget:    fixture.session.AttemptAllowance(),
		StartStep:      fixture.session.CompletedSteps(),
		StartedAt:      time.Now().UTC(),
		LeaseDuration:  terminalCheckpointLease,
	})
	if err != nil {
		t.Fatalf("StartOwnedAttempt: %v", err)
	}
	if err := fixture.session.BindAttempt(fixture.run.ID, fixture.run.OwnerToken); err != nil {
		t.Fatalf("BindAttempt: %v", err)
	}
	initialCheckpoint, err := fixture.session.Checkpoint()
	if err != nil {
		t.Fatalf("initial Checkpoint: %v", err)
	}
	if err := fixture.attempts.UpdateProgress(store.AttemptProgress{
		RunID:      fixture.run.ID,
		OwnerToken: fixture.run.OwnerToken,
		Checkpoint: initialCheckpoint,
	}); err != nil {
		t.Fatalf("publish initial checkpoint: %v", err)
	}
	return fixture
}

func (f *terminalCheckpointFixture) runToExhaustion(
	rejectTerminal bool,
) (agent.Outcome, error) {
	var output []string
	return f.session.RunFenced(
		context.Background(),
		func() error {
			return f.attempts.RenewLease(
				f.run.ID,
				f.run.OwnerToken,
				terminalCheckpointLease,
			)
		},
		func(change agent.StateCommit) error {
			if change.Event == nil && rejectTerminal {
				return errCrashBeforeTerminalCommit
			}
			if change.Event != nil {
				output = append(output, change.Event.Output)
			}
			return f.attempts.UpdateProgress(store.AttemptProgress{
				RunID:      f.run.ID,
				OwnerToken: f.run.OwnerToken,
				Output:     strings.Join(output, "\n"),
				TokensUsed: change.TokensUsed,
				Checkpoint: change.Checkpoint,
			})
		},
	)
}

func (f *terminalCheckpointFixture) startRecoveryServer(t *testing.T) *fakeClock {
	t.Helper()
	time.Sleep(2 * terminalCheckpointLease)
	clock := newFakeClock(f.recoveryAt)
	srv, err := New(
		Config{ResetInterval: f.resetInterval, ScanInterval: 2 * f.resetInterval},
		Dependencies{
			DataDir:       f.dataDir,
			Tasks:         f.tasks,
			Attempts:      f.attempts,
			Continuations: store.NewContinuationRepo(f.db),
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
	return clock
}

func waitForServer(t *testing.T, clock *fakeClock, description string) time.Duration {
	t.Helper()
	select {
	case wait := <-clock.waiting:
		return wait
	case <-time.After(time.Second):
		t.Fatalf("server did not %s", description)
		return 0
	}
}

func TestServerContinuesPartialWindowAfterPreTerminalCommitCrash(t *testing.T) {
	fixture := newTerminalCheckpointFixture(t, 0)
	_, err := fixture.runToExhaustion(true)
	if !errors.Is(err, errCrashBeforeTerminalCommit) {
		t.Fatalf("RunFenced error = %v, want simulated pre-terminal crash", err)
	}
	if got := fixture.session.AttemptCheckpoint().HaltReason; got != agent.HaltRunning {
		t.Fatalf("rolled-back halt reason = %q, want %q", got, agent.HaltRunning)
	}
	if fixture.session.CompletedSteps() != 1 {
		t.Fatalf("completed steps = %d, want 1", fixture.session.CompletedSteps())
	}

	clock := fixture.startRecoveryServer(t)
	if wait := waitForServer(t, clock, "recover and continue the partial window"); wait != fixture.resetInterval {
		t.Errorf("wait after same-window exhaustion = %s, want %s", wait, fixture.resetInterval)
	}

	history, err := fixture.runs.ListByTask(fixture.task.ID)
	if err != nil {
		t.Fatalf("ListByTask after recovery: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("runs after recovery = %d, want expired run and same-window continuation", len(history))
	}
	if history[0].ExitReason != domain.ExitInterrupted {
		t.Errorf("recovered exit reason = %q, want %q", history[0].ExitReason, domain.ExitInterrupted)
	}
	if history[1].ExitReason != domain.ExitTokenBudgetExhausted {
		t.Errorf(
			"same-window exit reason = %q, want %q",
			history[1].ExitReason,
			domain.ExitTokenBudgetExhausted,
		)
	}
	recovered, err := fixture.tasks.Get(fixture.task.ID)
	if err != nil {
		t.Fatalf("Get after same-window exhaustion: %v", err)
	}
	if recovered.Continuation.Kind() != domain.ContinuationScheduled {
		t.Errorf(
			"same-window continuation = %q (%s), want scheduled",
			recovered.Continuation.Kind(),
			recovered.Continuation.Reason(),
		)
	}
	wantEligible := fixture.recoveryAt.Add(fixture.resetInterval)
	if eligible := recovered.Continuation.EligibleAt(); eligible == nil || !eligible.Equal(wantEligible) {
		t.Errorf("eligibility = %v, want %s", eligible, wantEligible)
	}

	clock.Advance(fixture.resetInterval)
	waitForServer(t, clock, "continue after the provider reset")
	completed, err := fixture.tasks.Get(fixture.task.ID)
	if err != nil {
		t.Fatalf("Get after reset continuation: %v", err)
	}
	if completed.Status != domain.TaskDone {
		t.Errorf("status after fresh reset window = %q, want done", completed.Status)
	}
	if completed.Continuation.Kind() == domain.ContinuationStopped {
		t.Errorf("task entered stopped state: %s", completed.Continuation.Reason())
	}
	history, err = fixture.runs.ListByTask(fixture.task.ID)
	if err != nil {
		t.Fatalf("ListByTask after reset: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("runs after reset = %d, want expired, same-window, and fresh-window runs", len(history))
	}
}

func TestServerRecoversTokenExhaustionPublishedBeforeFinalization(t *testing.T) {
	fixture := newTerminalCheckpointFixture(t, 0)
	outcome, err := fixture.runToExhaustion(false)
	if err != nil {
		t.Fatalf("RunFenced: %v", err)
	}
	if outcome.Kind != agent.TokenBudgetExhausted {
		t.Fatalf("outcome = %d, want token exhaustion", outcome.Kind)
	}
	if fixture.session.CompletedSteps() != 1 {
		t.Fatalf("completed steps = %d, want 1", fixture.session.CompletedSteps())
	}

	clock := fixture.startRecoveryServer(t)
	if wait := waitForServer(t, clock, "recover the expired attempt"); wait != fixture.resetInterval {
		t.Errorf("wait after recovery = %s, want reset interval %s", wait, fixture.resetInterval)
	}

	history, err := fixture.runs.ListByTask(fixture.task.ID)
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
	recovered, err := fixture.tasks.Get(fixture.task.ID)
	if err != nil {
		t.Fatalf("Get after recovery: %v", err)
	}
	wantEligible := fixture.recoveryAt.Add(fixture.resetInterval)
	if eligible := recovered.Continuation.EligibleAt(); eligible == nil || !eligible.Equal(wantEligible) {
		t.Errorf("eligibility = %v, want %s", eligible, wantEligible)
	}
	if recovered.Continuation.Kind() == domain.ContinuationStopped {
		t.Errorf("continuation stopped after recovery: %s", recovered.Continuation.Reason())
	}

	clock.Advance(fixture.resetInterval)
	waitForServer(t, clock, "continue after the provider reset")
	completed, err := fixture.tasks.Get(fixture.task.ID)
	if err != nil {
		t.Fatalf("Get after continuation: %v", err)
	}
	if completed.Status != domain.TaskDone {
		t.Errorf("status after reset = %q, want done", completed.Status)
	}
	if completed.Continuation.Kind() == domain.ContinuationStopped {
		t.Errorf("completed task entered stopped state: %s", completed.Continuation.Reason())
	}
	history, err = fixture.runs.ListByTask(fixture.task.ID)
	if err != nil {
		t.Fatalf("ListByTask after continuation: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("runs after continuation = %d, want expired attempt and reset continuation", len(history))
	}
}

func TestServerUsesOriginalExhaustionTimeAfterLongDowntime(t *testing.T) {
	fixture := newTerminalCheckpointFixture(t, 2*time.Hour)
	outcome, err := fixture.runToExhaustion(false)
	if err != nil {
		t.Fatalf("RunFenced: %v", err)
	}
	if outcome.Kind != agent.TokenBudgetExhausted {
		t.Fatalf("outcome = %d, want token exhaustion", outcome.Kind)
	}

	clock := fixture.startRecoveryServer(t)
	if wait := waitForServer(t, clock, "continue after restart"); wait != 2*fixture.resetInterval {
		t.Errorf("wait after overdue reset = %s, want scan interval %s", wait, 2*fixture.resetInterval)
	}
	completed, err := fixture.tasks.Get(fixture.task.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if completed.Status != domain.TaskDone {
		t.Errorf("status after overdue reset = %q, want done", completed.Status)
	}
	history, err := fixture.runs.ListByTask(fixture.task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("runs after restart = %d, want recovered exhaustion and continuation", len(history))
	}
}
