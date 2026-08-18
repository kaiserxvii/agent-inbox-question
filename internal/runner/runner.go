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
	DataDir string
	Tasks   *store.TaskRepo
	Runs    *store.RunRepo
	Output  io.Writer
	NoDelay bool
}

// AttemptResult is the recorded outcome callers can act on without re-reading
// task and run state.
type AttemptResult struct {
	TaskStatus domain.TaskStatus
	RunStatus  domain.RunStatus
	ExitReason domain.ExitReason
}

func Execute(ctx context.Context, deps Deps, taskID int64) error {
	task, err := deps.Tasks.Get(taskID)
	if err != nil {
		return fmt.Errorf("get task %d: %w", taskID, err)
	}

	if err := deps.Tasks.Transition(taskID, domain.TaskTodo, domain.TaskInProgress); err != nil {
		return fmt.Errorf("claim task %d: %w", taskID, err)
	}

	var opts []agent.Option
	if deps.NoDelay {
		opts = append(opts, agent.WithNoDelay())
	}
	session, err := agent.Start(deps.DataDir, task.Title, task.Description, nil, opts...)
	if err != nil {
		return restoreFailedTask(
			deps,
			taskID,
			fmt.Errorf("start agent session: %w", err),
		)
	}

	_, err = runClaimedSession(ctx, deps, taskID, session)
	return err
}

// Resume atomically claims a failed task and records a new attempt against its
// persisted agent session.
func Resume(ctx context.Context, deps Deps, taskID int64) (AttemptResult, error) {
	task, err := deps.Tasks.Get(taskID)
	if err != nil {
		return AttemptResult{}, fmt.Errorf("get task %d: %w", taskID, err)
	}
	if task.Status != domain.TaskFailed {
		if task.Status == domain.TaskInProgress {
			return AttemptResult{}, fmt.Errorf(
				"%w: task %d cannot be resumed: status is %q",
				domain.ErrConflict,
				taskID,
				task.Status,
			)
		}
		return AttemptResult{}, fmt.Errorf("task %d cannot be resumed: status is %q", taskID, task.Status)
	}

	if err := deps.Tasks.Transition(taskID, domain.TaskFailed, domain.TaskInProgress); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			current, getErr := deps.Tasks.Get(taskID)
			if getErr == nil {
				return AttemptResult{}, fmt.Errorf(
					"%w: task %d cannot be resumed: status is %q",
					domain.ErrConflict,
					taskID,
					current.Status,
				)
			}
			return AttemptResult{}, errors.Join(
				fmt.Errorf("claim task %d for resume: %w", taskID, err),
				fmt.Errorf("get current task status: %w", getErr),
			)
		}
		return AttemptResult{}, fmt.Errorf("claim task %d for resume: %w", taskID, err)
	}

	runs, err := deps.Runs.ListByTask(taskID)
	if err != nil {
		return AttemptResult{}, restoreFailedTask(
			deps,
			taskID,
			fmt.Errorf("list runs for task %d: %w", taskID, err),
		)
	}
	if len(runs) == 0 {
		return AttemptResult{}, restoreFailedTask(
			deps,
			taskID,
			fmt.Errorf("task %d has no run to resume", taskID),
		)
	}

	var opts []agent.Option
	if deps.NoDelay {
		opts = append(opts, agent.WithNoDelay())
	}
	previous := runs[len(runs)-1]
	session, err := agent.Load(deps.DataDir, previous.SessionID, opts...)
	if err != nil {
		return AttemptResult{}, restoreFailedTask(
			deps,
			taskID,
			fmt.Errorf("load agent session: %w", err),
		)
	}

	return runClaimedSession(ctx, deps, taskID, session)
}

func runClaimedSession(ctx context.Context, deps Deps, taskID int64, session *agent.Session) (AttemptResult, error) {
	run, err := deps.Runs.Create(taskID, session.ID(), session.Budget())
	if err != nil {
		return AttemptResult{}, restoreFailedTask(
			deps,
			taskID,
			fmt.Errorf("create run: %w", err),
		)
	}

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
			if err := deps.Runs.UpdateProgress(run.ID, accumulated, e.TokensUsed); err != nil {
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
		params.RunStatus = domain.RunSucceeded
		params.ExitReason = domain.ExitCompleted
		params.TaskStatus = domain.TaskDone
		params.CommentAuthor = "agent"
		params.CommentBody = agent.Summary(session)
		result = AttemptResult{
			TaskStatus: domain.TaskDone,
			RunStatus:  domain.RunSucceeded,
			ExitReason: domain.ExitCompleted,
		}

	case agent.Errored:
		errMsg := ""
		if outcome.Err != nil {
			errMsg = outcome.Err.Error()
		}
		params.RunStatus = domain.RunErrored
		params.ExitReason = domain.ExitAgentError
		params.Error = errMsg
		params.TaskStatus = domain.TaskFailed
		result = AttemptResult{
			TaskStatus: domain.TaskFailed,
			RunStatus:  domain.RunErrored,
			ExitReason: domain.ExitAgentError,
		}

	case agent.TokenBudgetExhausted:
		params.RunStatus = domain.RunTokenExhausted
		params.ExitReason = domain.ExitTokenBudgetExhausted
		params.TaskStatus = domain.TaskFailed
		result = AttemptResult{
			TaskStatus: domain.TaskFailed,
			RunStatus:  domain.RunTokenExhausted,
			ExitReason: domain.ExitTokenBudgetExhausted,
		}

	default:
		if runErr == nil {
			runErr = fmt.Errorf("unknown agent outcome: %d", outcome.Kind)
		}
	}

	if runErr != nil {
		params.RunStatus = domain.RunErrored
		params.ExitReason = domain.ExitAgentError
		params.Error = runErr.Error()
		params.TaskStatus = domain.TaskFailed
		result = AttemptResult{}
	}

	if err := deps.Runs.FinishAttempt(params); err != nil {
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

func restoreFailedTask(deps Deps, taskID int64, cause error) error {
	if err := deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskFailed); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("restore task %d to failed: %w", taskID, err),
		)
	}
	return cause
}
