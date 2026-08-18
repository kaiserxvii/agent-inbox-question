package cli

import (
	"fmt"
)

func (a *App) RunAdd(args []string) error {
	var title, desc string
	for i := 0; i < len(args); i++ {
		if args[i] == "-d" && i+1 < len(args) {
			desc = args[i+1]
			i++
		} else if title == "" {
			title = args[i]
		}
	}

	if title == "" {
		return fmt.Errorf("usage: agent-inbox add <title> [-d description]")
	}

	task, err := a.Tasks.Create(title, desc)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	if _, err := fmt.Fprintln(a.output(), task.ID); err != nil {
		return fmt.Errorf("write task ID: %w", err)
	}
	return nil
}
