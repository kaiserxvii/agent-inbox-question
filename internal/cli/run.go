package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func (a *App) RunRun(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agent-inbox run <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid task id: %q", args[0])
	}

	deps := runner.Deps{
		DataDir: a.DataDir,
		Tasks:   a.Tasks,
		Runs:    a.Runs,
		Output:  a.output(),
		NoDelay: a.noDelay,
	}

	return runner.Execute(ctx, deps, id)
}
