package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func TestRunResumeReportsRecordedFailure(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close app: %v", err)
		}
	})

	task, err := app.Tasks.Create("very long task", "[steps:30] [budget:1]")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	deps := runner.Deps{
		DataDir:  app.DataDir,
		Tasks:    app.Tasks,
		Attempts: app.Attempts,
		Options:  runner.Options{NoDelay: true},
	}
	if err := runner.Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var output bytes.Buffer
	app.Stdout = &output
	app.RunnerOptions = runner.Options{NoDelay: true}
	if err := app.RunResume(context.Background(), []string{fmt.Sprint(task.ID)}); err != nil {
		t.Fatalf("RunResume: %v", err)
	}

	want := fmt.Sprintf("Task #%d: failed (token_budget_exhausted)", task.ID)
	if !strings.Contains(output.String(), want) {
		t.Errorf("resume output does not contain %q:\n%s", want, output.String())
	}
}
