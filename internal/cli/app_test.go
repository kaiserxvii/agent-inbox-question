package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func TestRunAddWritesTaskIDToConfiguredStdout(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	var stdout bytes.Buffer
	app.Stdout = &stdout
	if err := app.RunAdd([]string{"new task"}); err != nil {
		t.Fatalf("RunAdd: %v", err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("configured stdout received no task ID")
	}
}

func TestRunListWritesTableToConfiguredStdout(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := app.Tasks.Create("listed task", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stdout bytes.Buffer
	app.Stdout = &stdout
	if err := app.RunList(nil); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(stdout.String(), "listed task") {
		t.Errorf("configured stdout does not contain task table:\n%s", stdout.String())
	}
}

func TestRunStatusWritesCountsToConfiguredStdout(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	var stdout bytes.Buffer
	app.Stdout = &stdout
	if err := app.RunStatus(); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if !strings.Contains(stdout.String(), "total") {
		t.Errorf("configured stdout does not contain status counts:\n%s", stdout.String())
	}
}

func TestRunStatusShowsNextAutomaticContinuation(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	task, err := app.Tasks.Create("scheduled task", "[steps:2] [budget:400]")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	if err := runner.Execute(context.Background(), runner.Deps{
		DataDir:  app.DataDir,
		Tasks:    app.Tasks,
		Attempts: app.Attempts,
		Options: runner.Options{
			NoDelay:       true,
			Now:           func() time.Time { return now },
			ResetInterval: time.Hour,
		},
	}, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var output bytes.Buffer
	app.Stdout = &output
	if err := app.RunStatus(); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	shown := output.String()
	want := "Next automatic continuation: task #1 at 2026-08-17T13:00:00Z"
	if !strings.Contains(shown, want) {
		t.Errorf("status does not contain %q:\n%s", want, shown)
	}
	if !strings.Contains(shown, "Durable state does not prove a server process is alive") {
		t.Errorf("status omits server-liveness limitation:\n%s", shown)
	}
}

func TestRunWorkUsesConfiguredStreamsAndRunnerOptions(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := app.Tasks.Create("work task", "[steps:1] [budget:5000]"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app.Stdout = &stdout
	app.Stderr = &stderr
	app.RunnerOptions = runner.Options{NoDelay: true}
	if err := app.RunWork(context.Background()); err != nil {
		t.Fatalf("RunWork: %v", err)
	}
	if !strings.Contains(stdout.String(), "[step 1/1]") {
		t.Errorf("configured stdout does not contain agent output:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Running task") ||
		!strings.Contains(stderr.String(), "Work complete") {
		t.Errorf("configured stderr does not contain work progress and summary:\n%s", stderr.String())
	}
}
