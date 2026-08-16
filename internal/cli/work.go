package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func (a *App) RunWork(ctx context.Context) error {
	deps := runner.Deps{
		DataDir:  a.DataDir,
		Tasks:    a.Tasks,
		Runs:     a.Runs,
		Comments: a.Comments,
		Output:   os.Stdout,
	}

	var total, succeeded, failed int

	for {
		select {
		case <-ctx.Done():
			printWorkSummary(total, succeeded, failed)
			return nil
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
		fmt.Fprintf(os.Stderr, ">>> Running task #%d: %s\n", task.ID, task.Title)

		err = runner.Execute(ctx, deps, task.ID, domain.TaskTodo)

		updated, _ := a.Tasks.Get(task.ID)
		if updated != nil && updated.Status == domain.TaskDone {
			succeeded++
			fmt.Fprintf(os.Stderr, "<<< Task #%d: done\n", task.ID)
		} else {
			failed++
			status := "unknown"
			if updated != nil {
				status = string(updated.Status)
			}
			fmt.Fprintf(os.Stderr, "<<< Task #%d: %s\n", task.ID, status)
		}

		if err != nil && ctx.Err() != nil {
			break
		}
	}

	printWorkSummary(total, succeeded, failed)
	return nil
}

func printWorkSummary(total, succeeded, failed int) {
	fmt.Fprintf(os.Stderr, "\nWork complete: %d tasks processed, %d succeeded, %d failed\n", total, succeeded, failed)
}
