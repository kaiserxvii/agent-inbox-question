package cli

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	app, err := NewApp(dir)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	return app
}

func TestSmokeAddRunShowContinue(t *testing.T) {
	app := testApp(t)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := app.RunAdd([]string{"test task", "-d", "[steps:3] [budget:5000]"})

	w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("RunAdd: %v", err)
	}

	buf := make([]byte, 256)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	id, err := strconv.ParseInt(trimNewline(output), 10, 64)
	if err != nil {
		t.Fatalf("parse task id from output %q: %v", output, err)
	}

	task, _ := app.Tasks.Get(id)
	if task.Status != domain.TaskTodo {
		t.Errorf("status = %q, want todo", task.Status)
	}

	if err := app.RunRun(context.Background(), []string{strconv.FormatInt(id, 10)}); err != nil {
		t.Fatalf("RunRun: %v", err)
	}

	task, _ = app.Tasks.Get(id)
	if task.Status != domain.TaskDone {
		t.Fatalf("after run, status = %q, want done", task.Status)
	}

	old = os.Stdout
	r2, w2, _ := os.Pipe()
	os.Stdout = w2

	if err := app.RunShow([]string{strconv.FormatInt(id, 10)}); err != nil {
		w2.Close()
		os.Stdout = old
		t.Fatalf("RunShow: %v", err)
	}
	w2.Close()
	os.Stdout = old

	showBuf := make([]byte, 4096)
	n2, _ := r2.Read(showBuf)
	showOutput := string(showBuf[:n2])
	if len(showOutput) < 20 {
		t.Errorf("show output too short: %q", showOutput)
	}

	if err := app.RunContinue(context.Background(), []string{strconv.FormatInt(id, 10), "-m", "add error handling"}); err != nil {
		t.Fatalf("RunContinue: %v", err)
	}

	task, _ = app.Tasks.Get(id)
	if task.Status != domain.TaskDone {
		t.Errorf("after continue, status = %q, want done", task.Status)
	}

	runs, _ := app.Runs.ListByTask(id)
	if len(runs) != 2 {
		t.Errorf("runs = %d, want 2", len(runs))
	}
}

func TestListAndStatus(t *testing.T) {
	app := testApp(t)

	app.Tasks.Create("task a", "")
	app.Tasks.Create("task b", "")

	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w

	if err := app.RunList(nil); err != nil {
		w.Close()
		os.Stdout = old
		t.Fatalf("RunList: %v", err)
	}
	if err := app.RunStatus(); err != nil {
		w.Close()
		os.Stdout = old
		t.Fatalf("RunStatus: %v", err)
	}

	w.Close()
	os.Stdout = old
}

func TestContinueWrongStatus(t *testing.T) {
	app := testApp(t)

	task, _ := app.Tasks.Create("t", "")
	err := app.RunContinue(context.Background(), []string{strconv.FormatInt(task.ID, 10), "-m", "feedback"})
	if err == nil {
		t.Fatal("expected error for continue on todo task")
	}
}

func TestAddMissingTitle(t *testing.T) {
	app := testApp(t)
	err := app.RunAdd(nil)
	if err == nil {
		t.Fatal("expected error for missing title")
	}
}

func TestShowMissingID(t *testing.T) {
	app := testApp(t)
	err := app.RunShow(nil)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestRunMissingID(t *testing.T) {
	app := testApp(t)
	err := app.RunRun(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestContinueMissingMessage(t *testing.T) {
	app := testApp(t)
	task, _ := app.Tasks.Create("t", "")
	app.Tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress)
	app.Tasks.Transition(task.ID, domain.TaskInProgress, domain.TaskDone)

	err := app.RunContinue(context.Background(), []string{strconv.FormatInt(task.ID, 10)})
	if err == nil {
		t.Fatal("expected error for missing -m flag")
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
