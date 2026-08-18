package cli

import (
	"errors"
	"fmt"
	"time"

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

	current, err := a.Continuations.Current()
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("read current automatic continuation: %w", err)
	}
	if current != nil {
		out.Printf(
			"\nCurrent attempt: task #%d, run #%d, lease expires %s\n",
			current.TaskID,
			current.RunID,
			current.LeaseExpiresAt.Format(time.RFC3339),
		)
	}

	next, err := a.Continuations.Next()
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("read next automatic continuation: %w", err)
	}
	if next != nil {
		out.Printf(
			"Next automatic continuation: task #%d at %s\n",
			next.TaskID,
			next.EligibleAt.Format(time.RFC3339),
		)
	} else {
		out.Println("Automatic continuation: nothing eligible")
	}

	stopped, err := a.Continuations.Stopped()
	if err != nil {
		return fmt.Errorf("read stopped automatic continuations: %w", err)
	}
	for _, item := range stopped {
		out.Printf("Stopped automatic continuation: task #%d: %s\n", item.TaskID, item.Reason)
	}
	out.Println("Durable state does not prove a server process is alive.")

	return out.Err()
}
