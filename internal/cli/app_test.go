package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
