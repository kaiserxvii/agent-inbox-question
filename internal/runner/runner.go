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

func RecoverExpired(
	deps Deps,
	run *domain.Run,
	resetInterval time.Duration,
	now time.Time,
) error {
	session, err := agent.Load(deps.DataDir, run.SessionID)
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
		if err := session.BeginAttempt(run.TokenBudget); err != nil {
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

	params := store.FinishAttemptParams{
		RunID:          run.ID,
		TaskID:         run.TaskID,
		Outcome:        domain.AttemptInterrupted,
		Output:         checkpoint.Output,
		TokensUsed:     checkpoint.TokensUsed,
		Error:          "process exited before attempt finalization",
		FinishedAt:     now,
		LeaseCheckedAt: now,
		RecoverExpired: true,
	}
	switch checkpoint.HaltReason {
	case agent.HaltCompleted:
		params.Outcome = domain.AttemptCompleted
		params.Error = ""
		params.CommentAuthor = "agent"
		params.CommentBody = agent.Summary(session)
	case agent.HaltAgentError:
		params.Outcome = domain.AttemptAgentError
		params.Error = checkpoint.Error
	case agent.HaltTokenExhausted:
		params.Outcome = domain.AttemptTokenExhausted
		params.Error = ""
		completedDelta := checkpoint.CompletedSteps - run.StartStep
		if completedDelta == 0 {
			params.AutoRetryState = store.AutoRetryStopped
			params.AutoRetryReason = store.AutoRetryNoProgressReason
			break
		}
		next := now.Add(resetInterval)
		params.NextEligibleAt = &next
		params.AutoRetryState = store.AutoRetryScheduled
	case agent.HaltInterrupted, agent.HaltRunning, "":
		params.Outcome = domain.AttemptInterrupted
		next := now
		params.NextEligibleAt = &next
		params.AutoRetryState = store.AutoRetryScheduled
	default:
		return fmt.Errorf("reconcile run %d: unknown halt reason %q", run.ID, checkpoint.HaltReason)
	}

	if err := deps.Attempts.FinishAttempt(params); err != nil {
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
	if err := session.BindAttempt(run.ID, run.OwnerToken); err != nil {
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
	now := time.Now().UTC()
	if deps.Options.Now != nil {
		now = deps.Options.Now().UTC()
	}
	if candidate.NextEligibleAt != nil && now.Before(*candidate.NextEligibleAt) {
		return AttemptResult{}, fmt.Errorf(
			"task %d cannot be resumed until %s (%s remaining)",
			taskID,
			candidate.NextEligibleAt.Format(time.RFC3339),
			candidate.NextEligibleAt.Sub(now).Round(time.Second),
		)
	}

	var opts []agent.Option
	if deps.Options.NoDelay {
		opts = append(opts, agent.WithNoDelay())
	}
	session, err := agent.Load(deps.DataDir, candidate.SessionID, opts...)
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
		if err := session.BeginAttempt(allowance); err != nil {
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
	if err := session.BindAttempt(run.ID, run.OwnerToken); err != nil {
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
	var progressErr error
	outcome, runErr := session.Run(attemptCtx, func(e agent.Event) {
		outputLines = append(outputLines, e.Output)
		if deps.Output != nil && outputErr == nil {
			if _, err := fmt.Fprintln(deps.Output, e.Output); err != nil {
				outputErr = fmt.Errorf("write attempt output: %w", err)
			}
		}
		if progressErr == nil {
			accumulated := strings.Join(outputLines, "\n")
			if err := deps.Attempts.UpdateProgress(store.AttemptProgress{
				RunID:      run.ID,
				OwnerToken: run.OwnerToken,
				ObservedAt: time.Now().UTC(),
				Output:     accumulated,
				TokensUsed: e.TokensUsed,
			}); err != nil {
				progressErr = fmt.Errorf("update run progress: %w", err)
				if errors.Is(err, domain.ErrLeaseLost) {
					cancel()
				}
			}
		}
	})
	cancel()
	if leaseErr := <-leaseErrors; leaseErr != nil {
		return AttemptResult{}, leaseErr
	}
	if runErr != nil {
		return AttemptResult{}, errors.Join(
			fmt.Errorf("run agent session: %w", runErr),
			progressErr,
			outputErr,
		)
	}

	fullOutput := strings.Join(outputLines, "\n")
	params := store.FinishAttemptParams{
		RunID:      run.ID,
		TaskID:     taskID,
		OwnerToken: run.OwnerToken,
		Output:     fullOutput,
		TokensUsed: session.TokensUsed(),
	}
	if deps.Options.Now != nil {
		params.FinishedAt = deps.Options.Now().UTC()
	}
	params.LeaseCheckedAt = time.Now().UTC()
	result := AttemptResult{}
	switch outcome.Kind {
	case agent.Completed:
		params.Outcome = domain.AttemptCompleted
		params.CommentAuthor = "agent"
		params.CommentBody = agent.Summary(session)
		result = AttemptResult{Outcome: domain.AttemptCompleted}

	case agent.Errored:
		errMsg := ""
		if outcome.Err != nil {
			errMsg = outcome.Err.Error()
		}
		params.Outcome = domain.AttemptAgentError
		params.Error = errMsg
		result = AttemptResult{Outcome: domain.AttemptAgentError}

	case agent.TokenBudgetExhausted:
		params.Outcome = domain.AttemptTokenExhausted
		result = AttemptResult{Outcome: domain.AttemptTokenExhausted}
		if deps.Options.ResetInterval > 0 {
			completedSteps := session.CompletedSteps() - startStep
			if completedSteps == 0 {
				params.AutoRetryState = store.AutoRetryStopped
				params.AutoRetryReason = store.AutoRetryNoProgressReason
				break
			}
			now := time.Now().UTC()
			if deps.Options.Now != nil {
				now = deps.Options.Now().UTC()
			}
			next := now.Add(deps.Options.ResetInterval)
			params.NextEligibleAt = &next
			params.AutoRetryState = store.AutoRetryScheduled
		}

	case agent.Interrupted:
		params.Outcome = domain.AttemptInterrupted
		if outcome.Err != nil {
			params.Error = outcome.Err.Error()
		}
		now := time.Now().UTC()
		if deps.Options.Now != nil {
			now = deps.Options.Now().UTC()
		}
		params.NextEligibleAt = &now
		params.AutoRetryState = store.AutoRetryScheduled
		result = AttemptResult{Outcome: domain.AttemptInterrupted}

	default:
		return AttemptResult{}, fmt.Errorf("unknown agent outcome: %d", outcome.Kind)
	}

	if err := deps.Attempts.FinishAttempt(params); err != nil {
		return AttemptResult{}, errors.Join(
			fmt.Errorf("finish attempt: %w", err),
			progressErr,
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
		case now := <-ticker.C:
			if err := deps.Attempts.RenewLease(run.ID, run.OwnerToken, now.UTC(), duration); err != nil {
				renewal.cancel()
				renewal.result <- fmt.Errorf("renew attempt lease: %w", err)
				return
			}
		}
	}
}
