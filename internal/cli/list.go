package cli

import (
	"flag"
	"fmt"
	"strconv"

	"github.com/villagelabsco/agent-inbox/internal/domain"
)

func (a *App) RunList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	statusFlag := fs.String("status", "", "filter by status")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var statusFilter *domain.TaskStatus
	if *statusFlag != "" {
		s, err := domain.ParseTaskStatus(*statusFlag)
		if err != nil {
			return err
		}
		statusFilter = &s
	}

	tasks, err := a.Tasks.List(statusFilter)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	if len(tasks) == 0 {
		fmt.Println("No tasks.")
		return nil
	}

	var rows [][]string
	for _, t := range tasks {
		runCount, _ := a.Runs.CountByTask(t.ID)
		rows = append(rows, []string{
			strconv.FormatInt(t.ID, 10),
			string(t.Status),
			t.Title,
			strconv.Itoa(runCount),
			relativeTime(t.UpdatedAt),
		})
	}

	printTable([]string{"ID", "STATUS", "TITLE", "RUNS", "UPDATED"}, rows)
	return nil
}
