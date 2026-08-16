package runner

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/villagelabsco/agent-inbox/internal/agent"
	"github.com/villagelabsco/agent-inbox/internal/domain"
	"github.com/villagelabsco/agent-inbox/internal/store"
)

type Deps struct {
	DataDir  string
	Tasks    *store.TaskRepo
	Runs     *store.RunRepo
	Comments *store.CommentRepo
	Output   io.Writer
	NoDelay  bool
}

func Execute(ctx context.Context, deps Deps, taskID int64, fromStatus domain.TaskStatus) error {
	task, err := deps.Tasks.Get(taskID)
	if err != nil {
		return fmt.Errorf("get task %d: %w", taskID, err)
	}

	if err := deps.Tasks.Transition(taskID, fromStatus, domain.TaskInProgress); err != nil {
		return err
	}

	var feedback []string
	comments, err := deps.Comments.ListByTask(taskID)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	for _, c := range comments {
		if c.Author == "user" {
			feedback = append(feedback, c.Body)
		}
	}

	var opts []agent.Option
	if deps.NoDelay {
		opts = append(opts, agent.WithNoDelay())
	}
	session, err := agent.Start(deps.DataDir, task.Title, task.Description, feedback, opts...)
	if err != nil {
		deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskFailed)
		return fmt.Errorf("start agent session: %w", err)
	}

	run, err := deps.Runs.Create(taskID, session.ID(), session.Budget())
	if err != nil {
		deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskFailed)
		return fmt.Errorf("create run: %w", err)
	}

	var outputLines []string
	outcome, runErr := session.Run(ctx, func(e agent.Event) {
		outputLines = append(outputLines, e.Output)
		if deps.Output != nil {
			fmt.Fprintln(deps.Output, e.Output)
		}
		accumulated := strings.Join(outputLines, "\n")
		deps.Runs.UpdateProgress(run.ID, accumulated, e.TokensUsed)
	})

	fullOutput := strings.Join(outputLines, "\n")

	if runErr != nil {
		deps.Runs.Finish(run.ID, domain.RunErrored, domain.ExitAgentError, fullOutput, session.TokensUsed(), runErr.Error())
		deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskFailed)
		return fmt.Errorf("agent run failed: %w", runErr)
	}

	switch outcome.Kind {
	case agent.Completed:
		deps.Runs.Finish(run.ID, domain.RunSucceeded, domain.ExitCompleted, fullOutput, session.TokensUsed(), "")
		deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskDone)
		summary := agent.Summary(session)
		deps.Comments.Create(taskID, "agent", summary)

	case agent.Errored:
		errMsg := ""
		if outcome.Err != nil {
			errMsg = outcome.Err.Error()
		}
		deps.Runs.Finish(run.ID, domain.RunErrored, domain.ExitAgentError, fullOutput, session.TokensUsed(), errMsg)
		deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskFailed)

	case agent.TokenBudgetExhausted:
		deps.Runs.Finish(run.ID, domain.RunTokenExhausted, domain.ExitTokenBudgetExhausted, fullOutput, session.TokensUsed(), "")
		deps.Tasks.Transition(taskID, domain.TaskInProgress, domain.TaskFailed)
	}

	return nil
}
