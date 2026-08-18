package cli

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

func TestRunShowDisplaysOutputForEveryAttempt(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close app: %v", err)
		}
	})

	task, err := app.Tasks.Create("long task", "[steps:4] [budget:500]")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	deps := runner.Deps{
		DataDir: app.DataDir,
		Tasks:   app.Tasks,
		Runs:    app.Runs,
		NoDelay: true,
	}
	if err := runner.Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := runner.Resume(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	runs, err := app.Runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}

	var output bytes.Buffer
	app.out = &output
	if err := app.RunShow([]string{strconv.FormatInt(task.ID, 10)}); err != nil {
		t.Fatalf("RunShow: %v", err)
	}

	shown := output.String()
	for _, run := range runs {
		if run.Output == "" {
			t.Fatalf("run %d has no fixture output", run.ID)
		}
		if !strings.Contains(shown, run.Output) {
			t.Errorf("show output does not include run %d output %q:\n%s", run.ID, run.Output, shown)
		}
	}
}

type closingWriter struct {
	output bytes.Buffer
	close  func() error
	once   sync.Once
	err    error
}

func (w *closingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		w.err = w.close()
	})
	if w.err != nil {
		return 0, w.err
	}
	return w.output.Write(p)
}

func TestRunShowReportsUnavailableHistory(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	task, err := app.Tasks.Create("task", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	output := &closingWriter{close: app.Close}
	app.out = output
	err = app.RunShow([]string{strconv.FormatInt(task.ID, 10)})
	if err == nil {
		t.Fatal("RunShow returned nil, want history query error")
	}
	if !strings.Contains(err.Error(), "list comments") {
		t.Errorf("RunShow error = %q, want comment history context", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func TestRunShowReportsOutputFailure(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close app: %v", err)
		}
	})

	task, err := app.Tasks.Create("task", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	app.out = failingWriter{}

	err = app.RunShow([]string{strconv.FormatInt(task.ID, 10)})
	if err == nil {
		t.Fatal("RunShow returned nil, want output error")
	}
	if !strings.Contains(err.Error(), "write output") {
		t.Errorf("RunShow error = %q, want output context", err)
	}
}
