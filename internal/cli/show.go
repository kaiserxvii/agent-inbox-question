package cli

import (
	"fmt"
	"strconv"
	"strings"
)

func (a *App) RunShow(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agent-inbox show <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid task id: %q", args[0])
	}

	task, err := a.Tasks.Get(id)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}

	out := newOutputPrinter(a.output())
	out.Printf("Task #%d\n", task.ID)
	out.Printf("  Title:       %s\n", task.Title)
	out.Printf("  Description: %s\n", task.Description)
	out.Printf("  Status:      %s\n", task.Status)
	out.Printf("  Created:     %s\n", relativeTime(task.CreatedAt))
	out.Printf("  Updated:     %s\n", relativeTime(task.UpdatedAt))
	if err := out.Err(); err != nil {
		return err
	}

	comments, err := a.Comments.ListByTask(id)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	if len(comments) > 0 {
		out.Println()
		out.Println("Comments:")
		for _, c := range comments {
			out.Printf("  [%s] %s (%s)\n", c.Author, c.Body, relativeTime(c.CreatedAt))
		}
		if err := out.Err(); err != nil {
			return err
		}
	}

	runs, err := a.Runs.ListByTask(id)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(runs) > 0 {
		out.Println()
		out.Println("Runs:")
		for i, r := range runs {
			if i > 0 {
				out.Println()
			}
			finished := "running"
			if r.FinishedAt != nil {
				finished = relativeTime(*r.FinishedAt)
			}
			out.Printf("  Run #%d\n", r.ID)
			out.Printf("    Session:     %s\n", r.SessionID)
			out.Printf("    Status:      %s\n", r.Status)
			out.Printf("    Exit reason: %s\n", displayExitReason(string(r.ExitReason)))
			out.Printf("    Tokens:      %d / %d\n", r.TokensUsed, r.TokenBudget)
			out.Printf("    Started:     %s\n", relativeTime(r.StartedAt))
			out.Printf("    Finished:    %s\n", finished)
			if r.Error != "" {
				out.Printf("    Error:       %s\n", r.Error)
			}
			if r.Output != "" {
				out.Println()
				out.Println(strings.Repeat("─", 32) + fmt.Sprintf(" run #%d output ", r.ID) + strings.Repeat("─", 32))
				out.Println(r.Output)
			}
		}
	}

	return out.Err()
}

func displayExitReason(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
