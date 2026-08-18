package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/agent"
	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

type Deps struct {
	DataDir  string
	Tasks    *store.TaskRepo
	Attempts *store.AttemptRepo
	Output   io.Writer
	Options  Options
}

type Options struct {
	NoDelay            bool
	Now                func() time.Time
	ResetInterval      time.Duration
	LeaseDuration      time.Duration
	LeaseRenewInterval time.Duration
}

const (
	DefaultResetInterval      = 30 * time.Second
	defaultLeaseDuration      = 3 * time.Second
	defaultLeaseRenewInterval = time.Second
)

// AttemptResult is the recorded outcome callers can act on without re-reading
// task and run state.
type AttemptResult struct {
	Outcome domain.AttemptOutcome
}

type attemptExpectation struct {
	TaskID        int64
	Status        domain.TaskStatus
	PreviousRunID int64
}

type leaseRenewal struct {
	result chan<- error
	cancel context.CancelFunc
}

type attemptTerminalization struct {
	completion    domain.AttemptCompletion
	errorText     string
	commentAuthor string
	commentBody   string
}

func restoreAttemptSession(
	dataDir string,
	sessionID string,
	checkpoint string,
	opts ...agent.Option,
) (*agent.Session, error) {
	if checkpoint != "" {
		return agent.Restore(dataDir, checkpoint, opts...)
	}
	return agent.Load(dataDir, sessionID, opts...)
}

func bindOwnedSession(
	deps Deps,
	session *agent.Session,
	run *domain.Run,
) error {
	if err := session.BindAttempt(run.ID, run.OwnerToken); err != nil {
		return err
	}
	checkpoint, err := session.Checkpoint()
	if err != nil {
		return err
	}
	return deps.Attempts.UpdateProgress(store.AttemptProgress{
		RunID:      run.ID,
		OwnerToken: run.OwnerToken,
		TokensUsed: session.TokensUsed(),
		Checkpoint: checkpoint,
	})
}

func terminalizeCheckpoint(
	session *agent.Session,
	checkpoint agent.AttemptCheckpoint,
	startStep int,
	finishedAt time.Time,
	resetInterval time.Duration,
	interruptionError string,
) (attemptTerminalization, error) {
	terminal := attemptTerminalization{}
	var outcome domain.AttemptOutcome
	switch checkpoint.HaltReason {
	case agent.HaltCompleted:
		outcome = domain.AttemptCompleted
		terminal.commentAuthor = "agent"
		terminal.commentBody = agent.Summary(session)
	case agent.HaltAgentError:
		outcome = domain.AttemptAgentError
		terminal.errorText = checkpoint.Error
	case agent.HaltTokenExhausted:
		outcome = domain.AttemptTokenExhausted
	case agent.HaltInterrupted, agent.HaltRunning, "":
		outcome = domain.AttemptInterrupted
		terminal.errorText = interruptionError
	default:
		return attemptTerminalization{}, fmt.Errorf(
			"unknown attempt halt reason %q",
			checkpoint.HaltReason,
		)
	}
	completion, err := domain.DecideAttemptCompletion(domain.AttemptCompletionInput{
		Outcome:       outcome,
		FinishedAt:    finishedAt,
		ResetInterval: resetInterval,
		Progressed:    checkpoint.CompletedSteps > startStep,
		WindowOrigin:  checkpoint.WindowOrigin,
	})
	if err != nil {
		return attemptTerminalization{}, err
	}
	terminal.completion = completion
	return terminal, nil
}

func RecoverExpired(
	deps Deps,
	run *domain.Run,
	resetInterval time.Duration,
	now time.Time,
) error {
	session, err := restoreAttemptSession(
		deps.DataDir,
		run.SessionID,
		run.SessionCheckpoint,
	)
	if err != nil {
		return fmt.Errorf("load expired run session: %w", err)
	}
	checkpoint := session.AttemptCheckpoint()
	if checkpoint.RunID != run.ID {
		if checkpoint.RunID > run.ID {
			return fmt.Errorf(
				"reconcile run %d: session is bound to newer run %d",
				run.ID,
				checkpoint.RunID,
			)
		}
		if err := session.BeginAttempt(
			run.TokenBudget,
			domain.ProviderWindowUnknown,
		); err != nil {
			return fmt.Errorf("restore unbound run allowance: %w", err)
		}
		if err := session.BindAttempt(run.ID, run.OwnerToken); err != nil {
			return fmt.Errorf("bind expired run for reconciliation: %w", err)
		}
		checkpoint = session.AttemptCheckpoint()
	}
	if checkpoint.OwnerToken != run.OwnerToken {
		return fmt.Errorf("reconcile run %d: session owner token does not match", run.ID)
	}
	sessionCheckpoint, err := session.Checkpoint()
	if err != nil {
		return fmt.Errorf("encode expired run checkpoint: %w", err)
	}

	terminal, err := terminalizeCheckpoint(
		session,
		checkpoint,
		run.StartStep,
		now,
		resetInterval,
		"process exited before attempt finalization",
	)
	if err != nil {
		return fmt.Errorf("reconcile run %d: %w", run.ID, err)
	}
	params := store.FinishAttemptParams{
		RunID:             run.ID,
		TaskID:            run.TaskID,
		Completion:        terminal.completion,
		Output:            checkpoint.Output,
		TokensUsed:        checkpoint.TokensUsed,
		Error:             terminal.errorText,
		CommentAuthor:     terminal.commentAuthor,
		CommentBody:       terminal.commentBody,
		FinishedAt:        now,
		SessionCheckpoint: sessionCheckpoint,
	}

	if err := deps.Attempts.RecoverExpiredAttempt(params); err != nil {
		return fmt.Errorf("finalize expired run %d: %w", run.ID, err)
	}
	return nil
}

func Execute(ctx context.Context, deps Deps, taskID int64) error {
	task, err := deps.Tasks.Get(taskID)
	if err != nil {
		return fmt.Errorf("get task %d: %w", taskID, err)
	}

	if task.Status != domain.TaskTodo {
		return fmt.Errorf("execute task: %w", &domain.TaskStatusConflict{
			TaskID:   taskID,
			Expected: domain.TaskTodo,
			Observed: task.Status,
		})
	}

	var opts []agent.Option
	if deps.Options.NoDelay {
		opts = append(opts, agent.WithNoDelay())
	}
	session, err := agent.Start(deps.DataDir, task.Title, task.Description, nil, opts...)
	if err != nil {
		return fmt.Errorf("start agent session: %w", err)
	}

	run, err := startAttempt(
		deps,
		session,
		attemptExpectation{TaskID: taskID, Status: domain.TaskTodo},
	)
	if err != nil {
		return fmt.Errorf("start attempt for task %d: %w", taskID, err)
	}
	if err := bindOwnedSession(deps, session, run); err != nil {
		return fmt.Errorf("bind agent session to run %d: %w", run.ID, err)
	}

	_, err = runAttempt(ctx, deps, taskID, session, run)
	return err
}

// Resume atomically claims a failed task and records a new attempt against its
// persisted agent session.
func Resume(ctx context.Context, deps Deps, taskID int64) (AttemptResult, error) {
	candidate, err := deps.Attempts.GetResumeCandidate(taskID)
	if err != nil {
		return AttemptResult{}, fmt.Errorf("get resume candidate for task %d: %w", taskID, err)
	}
	if candidate.TaskStatus != domain.TaskFailed {
		return AttemptResult{}, fmt.Errorf(
			"%w: task %d cannot be resumed: status is %q",
			&domain.TaskStatusConflict{
				TaskID:   taskID,
				Expected: domain.TaskFailed,
				Observed: candidate.TaskStatus,
			},
			taskID,
			candidate.TaskStatus,
		)
	}
	if candidate.RunID == 0 {
		return AttemptResult{}, fmt.Errorf("task %d has no terminal run to resume", taskID)
	}
	if candidate.Continuation.Kind() == domain.ContinuationStopped {
		return AttemptResult{}, fmt.Errorf(
			"task %d cannot be resumed: %s",
			taskID,
			candidate.Continuation.Reason(),
		)
	}
	now := time.Now().UTC()
	if deps.Options.Now != nil {
		now = deps.Options.Now().UTC()
	}
	nextEligibleAt := candidate.Continuation.EligibleAt()
	if nextEligibleAt != nil && now.Before(*nextEligibleAt) {
		return AttemptResult{}, fmt.Errorf(
			"task %d cannot be resumed until %s (%s remaining)",
			taskID,
			nextEligibleAt.Format(time.RFC3339),
			nextEligibleAt.Sub(now).Round(time.Second),
		)
	}

	var opts []agent.Option
	if deps.Options.NoDelay {
		opts = append(opts, agent.WithNoDelay())
	}
	session, err := restoreAttemptSession(
		deps.DataDir,
		candidate.SessionID,
		candidate.SessionCheckpoint,
		opts...,
	)
	if err != nil {
		return AttemptResult{}, fmt.Errorf("load agent session: %w", err)
	}

	var allowance int
	switch candidate.ExitReason {
	case domain.ExitAgentError:
		allowance = session.ConfiguredBudget()
	case domain.ExitTokenBudgetExhausted:
		allowance = session.ConfiguredBudget()
	case domain.ExitInterrupted:
		session.ContinueAttempt()
		allowance = session.AttemptAllowance()
	default:
		return AttemptResult{}, fmt.Errorf(
			"task %d cannot resume run %d with exit reason %q",
			taskID,
			candidate.RunID,
			candidate.ExitReason,
		)
	}
	if candidate.ExitReason != domain.ExitInterrupted {
		if err := session.BeginAttempt(
			allowance,
			domain.ProviderWindowFresh,
		); err != nil {
			return AttemptResult{}, fmt.Errorf("begin resumed session attempt: %w", err)
		}
	}

	run, err := startAttempt(
		deps,
		session,
		attemptExpectation{
			TaskID:        taskID,
			Status:        domain.TaskFailed,
			PreviousRunID: candidate.RunID,
		},
	)
	if err != nil {
		var conflict *domain.TaskStatusConflict
		if errors.As(err, &conflict) {
			return AttemptResult{}, fmt.Errorf(
				"%w: task %d cannot be resumed: status is %q",
				conflict,
				taskID,
				conflict.Observed,
			)
		}
		return AttemptResult{}, fmt.Errorf("start resumed attempt for task %d: %w", taskID, err)
	}
	if err := bindOwnedSession(deps, session, run); err != nil {
		return AttemptResult{}, fmt.Errorf("bind resumed session to run %d: %w", run.ID, err)
	}

	return runAttempt(ctx, deps, taskID, session, run)
}

func runAttempt(ctx context.Context, deps Deps, taskID int64, session *agent.Session, run *domain.Run) (AttemptResult, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseErrors := make(chan error, 1)
	go renewLease(attemptCtx, deps, run, leaseRenewal{result: leaseErrors, cancel: cancel})

	startStep := session.CompletedSteps()
	var outputLines []string
	var outputErr error
	leaseDuration := deps.Options.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	outcome, runErr := session.RunFenced(attemptCtx, func() error {
		return deps.Attempts.RenewLease(
			run.ID,
			run.OwnerToken,
			leaseDuration,
		)
	}, func(change agent.StateCommit) error {
		accumulatedLines := append([]string(nil), outputLines...)
		if change.Event != nil {
			accumulatedLines = append(accumulatedLines, change.Event.Output)
		}
		if err := deps.Attempts.UpdateProgress(store.AttemptProgress{
			RunID:      run.ID,
			OwnerToken: run.OwnerToken,
			Output:     strings.Join(accumulatedLines, "\n"),
			TokensUsed: change.TokensUsed,
			Checkpoint: change.Checkpoint,
		}); err != nil {
			if errors.Is(err, domain.ErrLeaseLost) {
				cancel()
			}
			return fmt.Errorf("update run progress: %w", err)
		}
		outputLines = accumulatedLines
		if change.Event != nil && deps.Output != nil && outputErr == nil {
			if _, err := fmt.Fprintln(deps.Output, change.Event.Output); err != nil {
				outputErr = fmt.Errorf("write attempt output: %w", err)
			}
		}
		return nil
	})
	cancel()
	if leaseErr := <-leaseErrors; leaseErr != nil {
		return AttemptResult{}, leaseErr
	}
	if runErr != nil {
		return AttemptResult{}, errors.Join(
			fmt.Errorf("run agent session: %w", runErr),
			outputErr,
		)
	}

	fullOutput := strings.Join(outputLines, "\n")
	checkpoint, err := session.Checkpoint()
	if err != nil {
		return AttemptResult{}, fmt.Errorf("encode final session checkpoint: %w", err)
	}
	params := store.FinishAttemptParams{
		RunID:             run.ID,
		TaskID:            taskID,
		OwnerToken:        run.OwnerToken,
		Output:            fullOutput,
		TokensUsed:        session.TokensUsed(),
		SessionCheckpoint: checkpoint,
	}
	finishedAt := time.Now().UTC()
	if deps.Options.Now != nil {
		finishedAt = deps.Options.Now().UTC()
	}
	interruptionError := ""
	if outcome.Err != nil {
		interruptionError = outcome.Err.Error()
	}
	terminal, err := terminalizeCheckpoint(
		session,
		session.AttemptCheckpoint(),
		startStep,
		finishedAt,
		deps.Options.ResetInterval,
		interruptionError,
	)
	if err != nil {
		return AttemptResult{}, err
	}
	params.Completion = terminal.completion
	params.Error = terminal.errorText
	params.CommentAuthor = terminal.commentAuthor
	params.CommentBody = terminal.commentBody
	params.FinishedAt = finishedAt
	result := AttemptResult{Outcome: terminal.completion.Outcome()}

	if err := deps.Attempts.FinishAttempt(params); err != nil {
		return AttemptResult{}, errors.Join(
			fmt.Errorf("finish attempt: %w", err),
			outputErr,
		)
	}
	// A successful final write contains the complete output, repairing any
	// failed intermediate progress snapshot.
	if outputErr != nil {
		return AttemptResult{}, outputErr
	}

	return result, nil
}

func startAttempt(
	deps Deps,
	session *agent.Session,
	expected attemptExpectation,
) (*domain.Run, error) {
	leaseDuration := deps.Options.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	return deps.Attempts.StartOwnedAttempt(store.StartAttemptParams{
		TaskID:         expected.TaskID,
		ExpectedStatus: expected.Status,
		ExpectedRunID:  expected.PreviousRunID,
		SessionID:      session.ID(),
		TokenBudget:    session.AttemptAllowance(),
		StartStep:      session.CompletedSteps(),
		StartedAt:      time.Now().UTC(),
		LeaseDuration:  leaseDuration,
	})
}

func renewLease(
	ctx context.Context,
	deps Deps,
	run *domain.Run,
	renewal leaseRenewal,
) {
	interval := deps.Options.LeaseRenewInterval
	if interval <= 0 {
		interval = defaultLeaseRenewInterval
	}
	duration := deps.Options.LeaseDuration
	if duration <= 0 {
		duration = defaultLeaseDuration
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			renewal.result <- nil
			return
		case <-ticker.C:
			if err := deps.Attempts.RenewLease(run.ID, run.OwnerToken, duration); err != nil {
				renewal.cancel()
				renewal.result <- fmt.Errorf("renew attempt lease: %w", err)
				return
			}
		}
	}
}
