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
	out := newOutputPrinter(a.output())
	for _, s := range statuses {
		c := counts[s]
		total += c
		out.Printf("%-12s %d\n", s, c)
	}
	out.Printf("%-12s %d\n", "total", total)

	inProgress, err := a.Tasks.OldestByStatus(domain.TaskInProgress)
	if err == nil && inProgress != nil {
		out.Printf("\nIn progress: #%d %s\n", inProgress.ID, inProgress.Title)
	}

	return out.Err()
}
