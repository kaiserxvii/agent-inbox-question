package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
	NoDelay bool
}

// AttemptResult is the recorded outcome callers can act on without re-reading
// task and run state.
type AttemptResult struct {
	Outcome domain.AttemptOutcome
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

	run, err := deps.Attempts.StartAttempt(taskID, domain.TaskTodo, 0, session.ID(), session.AttemptAllowance())
	if err != nil {
		return fmt.Errorf("start attempt for task %d: %w", taskID, err)
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
		if candidate.TaskStatus == domain.TaskInProgress {
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
		return AttemptResult{}, fmt.Errorf(
			"task %d cannot be resumed: status is %q",
			taskID,
			candidate.TaskStatus,
		)
	}
	if candidate.RunID == 0 {
		return AttemptResult{}, fmt.Errorf("task %d has no terminal run to resume", taskID)
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
	default:
		return AttemptResult{}, fmt.Errorf(
			"task %d cannot resume run %d with exit reason %q",
			taskID,
			candidate.RunID,
			candidate.ExitReason,
		)
	}
	if err := session.BeginAttempt(allowance); err != nil {
		return AttemptResult{}, fmt.Errorf("begin resumed session attempt: %w", err)
	}

	run, err := deps.Attempts.StartAttempt(
		taskID,
		domain.TaskFailed,
		candidate.RunID,
		session.ID(),
		session.AttemptAllowance(),
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

	return runAttempt(ctx, deps, taskID, session, run)
}

func runAttempt(ctx context.Context, deps Deps, taskID int64, session *agent.Session, run *domain.Run) (AttemptResult, error) {
	var outputLines []string
	var outputErr error
	var progressErr error
	outcome, runErr := session.Run(ctx, func(e agent.Event) {
		outputLines = append(outputLines, e.Output)
		if deps.Output != nil && outputErr == nil {
			if _, err := fmt.Fprintln(deps.Output, e.Output); err != nil {
				outputErr = fmt.Errorf("write attempt output: %w", err)
			}
		}
		if progressErr == nil {
			accumulated := strings.Join(outputLines, "\n")
			if err := deps.Attempts.UpdateProgress(run.ID, accumulated, e.TokensUsed); err != nil {
				progressErr = fmt.Errorf("update run progress: %w", err)
			}
		}
	})

	fullOutput := strings.Join(outputLines, "\n")
	params := store.FinishAttemptParams{
		RunID:      run.ID,
		TaskID:     taskID,
		Output:     fullOutput,
		TokensUsed: session.TokensUsed(),
	}
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

	default:
		if runErr == nil {
			runErr = fmt.Errorf("unknown agent outcome: %d", outcome.Kind)
		}
	}

	if runErr != nil {
		params.Outcome = domain.AttemptAgentError
		params.Error = runErr.Error()
		result = AttemptResult{}
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
	if runErr != nil {
		return AttemptResult{}, errors.Join(
			fmt.Errorf("run agent session: %w", runErr),
			outputErr,
		)
	}
	if outputErr != nil {
		return AttemptResult{}, outputErr
	}

	return result, nil
}
