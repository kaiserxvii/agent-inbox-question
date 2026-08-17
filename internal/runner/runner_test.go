package runner

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

func setupTest(t *testing.T) (Deps, *store.TaskRepo, *store.RunRepo, *store.CommentRepo) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tasks := store.NewTaskRepo(db)
	runs := store.NewRunRepo(db)
	comments := store.NewCommentRepo(db)

	deps := Deps{
		DataDir:  dir,
		Tasks:    tasks,
		Runs:     runs,
		Comments: comments,
		Output:   &bytes.Buffer{},
		NoDelay:  true,
	}
	return deps, tasks, runs, comments
}

func TestExecuteSuccess(t *testing.T) {
	deps, tasks, runs, comments := setupTest(t)

	task, _ := tasks.Create("success task", "[steps:3] [budget:5000]")

	err := Execute(context.Background(), deps, task.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := tasks.Get(task.ID)
	if got.Status != domain.TaskDone {
		t.Errorf("task status = %q, want done", got.Status)
	}

	runList, _ := runs.ListByTask(task.ID)
	if len(runList) != 1 {
		t.Fatalf("runs = %d, want 1", len(runList))
	}
	r := runList[0]
	if r.Status != domain.RunSucceeded {
		t.Errorf("run status = %q, want succeeded", r.Status)
	}
	if r.ExitReason != domain.ExitCompleted {
		t.Errorf("exit_reason = %q, want completed", r.ExitReason)
	}
	if r.Output == "" {
		t.Error("run output is empty")
	}
	if r.TokensUsed == 0 {
		t.Error("tokens_used is 0")
	}
	if r.SessionID == "" {
		t.Error("session_id is empty")
	}
	if r.FinishedAt == nil {
		t.Error("finished_at is nil")
	}

	cmts, _ := comments.ListByTask(task.ID)
	found := false
	for _, c := range cmts {
		if c.Author == "agent" {
			found = true
		}
	}
	if !found {
		t.Error("no agent comment recorded")
	}
}

func TestExecuteFailAt(t *testing.T) {
	deps, tasks, runs, _ := setupTest(t)

	task, _ := tasks.Create("fail task", "[steps:5] [fail-at:2] [budget:5000]")

	Execute(context.Background(), deps, task.ID)

	got, _ := tasks.Get(task.ID)
	if got.Status != domain.TaskFailed {
		t.Errorf("task status = %q, want failed", got.Status)
	}

	runList, _ := runs.ListByTask(task.ID)
	if len(runList) != 1 {
		t.Fatalf("runs = %d, want 1", len(runList))
	}
	r := runList[0]
	if r.Status != domain.RunErrored {
		t.Errorf("run status = %q, want errored", r.Status)
	}
	if r.ExitReason != domain.ExitAgentError {
		t.Errorf("exit_reason = %q, want agent_error", r.ExitReason)
	}
	if r.Error == "" {
		t.Error("error text is empty")
	}
}

func TestExecuteTokenExhaustion(t *testing.T) {
	deps, tasks, runs, _ := setupTest(t)

	task, _ := tasks.Create("exhaust task", "[steps:10] [budget:1]")

	Execute(context.Background(), deps, task.ID)

	got, _ := tasks.Get(task.ID)
	if got.Status != domain.TaskFailed {
		t.Errorf("task status = %q, want failed", got.Status)
	}

	runList, _ := runs.ListByTask(task.ID)
	if len(runList) != 1 {
		t.Fatalf("runs = %d, want 1", len(runList))
	}
	r := runList[0]
	if r.Status != domain.RunTokenExhausted {
		t.Errorf("run status = %q, want token_exhausted", r.Status)
	}
	if r.ExitReason != domain.ExitTokenBudgetExhausted {
		t.Errorf("exit_reason = %q, want token_budget_exhausted", r.ExitReason)
	}
}

func TestExecuteConflict(t *testing.T) {
	deps, tasks, _, _ := setupTest(t)

	task, _ := tasks.Create("contested", "")
	tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress)

	err := Execute(context.Background(), deps, task.ID)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
}
