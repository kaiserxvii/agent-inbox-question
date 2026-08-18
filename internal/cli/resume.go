package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func (a *App) RunResume(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agent-inbox resume <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid task id: %q", args[0])
	}

	result, err := runner.Resume(ctx, runner.Deps{
		DataDir: a.DataDir,
		Tasks:   a.Tasks,
		Runs:    a.Runs,
		Output:  a.output(),
		NoDelay: a.noDelay,
	}, id)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(
		a.output(),
		"Task #%d: %s (%s)\n",
		id,
		result.TaskStatus,
		result.ExitReason,
	); err != nil {
		return fmt.Errorf("write resume result: %w", err)
	}
	return nil
}
