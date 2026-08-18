package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func (a *App) RunWork(ctx context.Context) error {
	deps := runner.Deps{
		DataDir:  a.DataDir,
		Tasks:    a.Tasks,
		Runs:     a.Runs,
		Attempts: a.Attempts,
		Output:   a.output(),
		Options:  a.RunnerOptions,
	}
	progress := newOutputPrinter(a.errorOutput())

	var total, succeeded, failed int

	for {
		select {
		case <-ctx.Done():
			printWorkSummary(progress, total, succeeded, failed)
			return progress.Err()
		default:
		}

		task, err := a.Tasks.OldestByStatus(domain.TaskTodo)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				break
			}
			return fmt.Errorf("find next task: %w", err)
		}

		total++
		progress.Printf(">>> Running task #%d: %s\n", task.ID, task.Title)

		err = runner.Execute(ctx, deps, task.ID)

		updated, _ := a.Tasks.Get(task.ID)
		if updated != nil && updated.Status == domain.TaskDone {
			succeeded++
			progress.Printf("<<< Task #%d: done\n", task.ID)
		} else {
			failed++
			status := "unknown"
			if updated != nil {
				status = string(updated.Status)
			}
			progress.Printf("<<< Task #%d: %s\n", task.ID, status)
		}

		if err != nil && ctx.Err() != nil {
			break
		}
	}

	printWorkSummary(progress, total, succeeded, failed)
	return progress.Err()
}

func printWorkSummary(out *outputPrinter, total, succeeded, failed int) {
	out.Printf("\nWork complete: %d tasks processed, %d succeeded, %d failed\n", total, succeeded, failed)
}
