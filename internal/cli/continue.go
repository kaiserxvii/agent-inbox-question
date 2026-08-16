package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func (a *App) RunContinue(ctx context.Context, args []string) error {
	var idStr, message string
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			message = args[i+1]
			i++
		} else if idStr == "" {
			idStr = args[i]
		}
	}

	if idStr == "" {
		return fmt.Errorf("usage: agent-inbox continue <id> -m <feedback>")
	}
	if message == "" {
		return fmt.Errorf("feedback message is required: agent-inbox continue <id> -m <feedback>")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid task id: %q", idStr)
	}

	task, err := a.Tasks.Get(id)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	if task.Status != domain.TaskDone {
		return fmt.Errorf("cannot continue task #%d: status is %q (must be %q)", id, task.Status, domain.TaskDone)
	}

	if _, err := a.Comments.Create(id, "user", message); err != nil {
		return fmt.Errorf("record feedback: %w", err)
	}

	deps := runner.Deps{
		DataDir:  a.DataDir,
		Tasks:    a.Tasks,
		Runs:     a.Runs,
		Comments: a.Comments,
		Output:   os.Stdout,
	}

	return runner.Execute(ctx, deps, id, domain.TaskDone)
}
