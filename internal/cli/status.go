package cli

import (
	"fmt"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

func (a *App) RunStatus() error {
	counts, err := a.Tasks.CountByStatus()
	if err != nil {
		return fmt.Errorf("count by status: %w", err)
	}

	statuses := []domain.TaskStatus{domain.TaskTodo, domain.TaskInProgress, domain.TaskDone, domain.TaskFailed}
	total := 0
	for _, s := range statuses {
		c := counts[s]
		total += c
		fmt.Printf("%-12s %d\n", s, c)
	}
	fmt.Printf("%-12s %d\n", "total", total)

	inProgress, err := a.Tasks.OldestByStatus(domain.TaskInProgress)
	if err == nil && inProgress != nil {
		fmt.Printf("\nIn progress: #%d %s\n", inProgress.ID, inProgress.Title)
	}

	return nil
}
