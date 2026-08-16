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

	fmt.Printf("Task #%d\n", task.ID)
	fmt.Printf("  Title:       %s\n", task.Title)
	fmt.Printf("  Description: %s\n", task.Description)
	fmt.Printf("  Status:      %s\n", task.Status)
	fmt.Printf("  Created:     %s\n", relativeTime(task.CreatedAt))
	fmt.Printf("  Updated:     %s\n", relativeTime(task.UpdatedAt))

	comments, _ := a.Comments.ListByTask(id)
	if len(comments) > 0 {
		fmt.Println()
		fmt.Println("Comments:")
		for _, c := range comments {
			fmt.Printf("  [%s] %s (%s)\n", c.Author, c.Body, relativeTime(c.CreatedAt))
		}
	}

	runs, _ := a.Runs.ListByTask(id)
	if len(runs) > 0 {
		fmt.Println()
		fmt.Println("Runs:")
		for _, r := range runs {
			finished := "running"
			if r.FinishedAt != nil {
				finished = relativeTime(*r.FinishedAt)
			}
			fmt.Printf("  Run #%d\n", r.ID)
			fmt.Printf("    Session:     %s\n", r.SessionID)
			fmt.Printf("    Status:      %s\n", r.Status)
			fmt.Printf("    Exit reason: %s\n", displayExitReason(string(r.ExitReason)))
			fmt.Printf("    Tokens:      %d / %d\n", r.TokensUsed, r.TokenBudget)
			fmt.Printf("    Started:     %s\n", relativeTime(r.StartedAt))
			fmt.Printf("    Finished:    %s\n", finished)
			if r.Error != "" {
				fmt.Printf("    Error:       %s\n", r.Error)
			}
		}

		latest := runs[len(runs)-1]
		if latest.Output != "" {
			fmt.Println()
			fmt.Println(strings.Repeat("─", 40) + " output " + strings.Repeat("─", 40))
			fmt.Println(latest.Output)
		}
	}

	return nil
}

func displayExitReason(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
