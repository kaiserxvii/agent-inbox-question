package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close database: %v", err)
		}
	})

	tasks := store.NewTaskRepo(db)
	runs := store.NewRunRepo(db)
	comments := store.NewCommentRepo(db)

	deps := Deps{
		DataDir:  dir,
		Tasks:    tasks,
		Runs:     runs,
		Attempts: store.NewAttemptRepo(db),
		Output:   &bytes.Buffer{},
		Options:  Options{NoDelay: true},
	}
	return deps, tasks, runs, comments
}

func createTask(t *testing.T, tasks *store.TaskRepo, title, description string) *domain.Task {
	t.Helper()
	task, err := tasks.Create(title, description)
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	return task
}

func getTask(t *testing.T, tasks *store.TaskRepo, taskID int64) *domain.Task {
	t.Helper()
	task, err := tasks.Get(taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	return task
}

func listRuns(t *testing.T, runs *store.RunRepo, taskID int64) []*domain.Run {
	t.Helper()
	runList, err := runs.ListByTask(taskID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	return runList
}

func listComments(t *testing.T, comments *store.CommentRepo, taskID int64) []*domain.Comment {
	t.Helper()
	commentList, err := comments.ListByTask(taskID)
	if err != nil {
		t.Fatalf("List comments: %v", err)
	}
	return commentList
}

func TestExecuteSuccess(t *testing.T) {
	deps, tasks, runs, comments := setupTest(t)

	task := createTask(t, tasks, "success task", "[steps:3] [budget:5000]")

	err := Execute(context.Background(), deps, task.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := getTask(t, tasks, task.ID)
	if got.Status != domain.TaskDone {
		t.Errorf("task status = %q, want done", got.Status)
	}

	runList := listRuns(t, runs, task.ID)
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

	cmts := listComments(t, comments, task.ID)
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

	task := createTask(t, tasks, "fail task", "[steps:5] [fail-at:2] [budget:5000]")

	Execute(context.Background(), deps, task.ID)

	got := getTask(t, tasks, task.ID)
	if got.Status != domain.TaskFailed {
		t.Errorf("task status = %q, want failed", got.Status)
	}

	runList := listRuns(t, runs, task.ID)
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

	task := createTask(t, tasks, "exhaust task", "[steps:10] [budget:1]")

	Execute(context.Background(), deps, task.ID)

	got := getTask(t, tasks, task.ID)
	if got.Status != domain.TaskFailed {
		t.Errorf("task status = %q, want failed", got.Status)
	}

	runList := listRuns(t, runs, task.ID)
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

func TestResumeContinuesTokenExhaustedTaskWithinConfiguredAttemptBudget(t *testing.T) {
	deps, tasks, runs, _ := setupTest(t)

	task := createTask(t, tasks, "long task", "[steps:4] [budget:500]")
	if err := Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	before, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask before resume: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("runs before resume = %d, want 1", len(before))
	}
	firstOutput := before[0].Output
	completedBefore := strings.Count(firstOutput, "[step ")
	if completedBefore == 0 {
		t.Fatal("initial attempt completed no work; test requires persisted progress")
	}
	if before[0].Status != domain.RunTokenExhausted {
		t.Fatalf("initial run status = %q, want token_exhausted", before[0].Status)
	}

	for attempt := 0; attempt < 4; attempt++ {
		if _, err := Resume(context.Background(), deps, task.ID); err != nil {
			t.Fatalf("Resume attempt %d: %v", attempt+2, err)
		}
		got := getTask(t, tasks, task.ID)
		if got.Status == domain.TaskDone {
			break
		}
	}

	got := getTask(t, tasks, task.ID)
	if got.Status != domain.TaskDone {
		t.Errorf("task status = %q, want done", got.Status)
	}

	after := listRuns(t, runs, task.ID)
	if len(after) < 2 {
		t.Fatalf("runs after resume = %d, want at least 2", len(after))
	}
	if after[0].Output != firstOutput {
		t.Errorf("first run output was modified:\n got: %q\nwant: %q", after[0].Output, firstOutput)
	}
	completed := 0
	for _, run := range after {
		if run.SessionID != after[0].SessionID {
			t.Errorf("run %d session = %q, want original session %q", run.ID, run.SessionID, after[0].SessionID)
		}
		if run.TokenBudget != 500 {
			t.Errorf("run %d budget = %d, want configured budget 500", run.ID, run.TokenBudget)
		}
		completed += strings.Count(run.Output, "[step ")
	}
	if after[len(after)-1].Status != domain.RunSucceeded {
		t.Errorf("last run status = %q, want succeeded", after[len(after)-1].Status)
	}
	if completed != 4 {
		t.Errorf("completed steps across attempts = %d, want 4", completed)
	}
}

func TestResumeRejectsTasksThatAreNotFailed(t *testing.T) {
	tests := []domain.TaskStatus{
		domain.TaskTodo,
		domain.TaskInProgress,
		domain.TaskDone,
	}

	for _, status := range tests {
		t.Run(string(status), func(t *testing.T) {
			deps, tasks, runs, _ := setupTest(t)
			task := createTask(t, tasks, "not resumable", "")

			if status != domain.TaskTodo {
				if err := tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress); err != nil {
					t.Fatalf("transition to in_progress: %v", err)
				}
			}
			if status == domain.TaskDone {
				if err := tasks.Transition(task.ID, domain.TaskInProgress, domain.TaskDone); err != nil {
					t.Fatalf("transition to done: %v", err)
				}
			}

			_, err := Resume(context.Background(), deps, task.ID)
			if err == nil {
				t.Fatal("Resume returned nil, want status error")
			}
			want := fmt.Sprintf("status is %q", status)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Resume error = %q, want it to contain %q", err, want)
			}

			count, err := runs.CountByTask(task.ID)
			if err != nil {
				t.Fatalf("CountByTask: %v", err)
			}
			if count != 0 {
				t.Errorf("runs = %d, want 0", count)
			}
		})
	}
}

func TestResumeRetriesTransientAgentErrorWithoutRepeatingCompletedWork(t *testing.T) {
	deps, tasks, runs, _ := setupTest(t)

	task := createTask(t, tasks, "transient failure", "[steps:4] [fail-at:2] [budget:5000]")
	if err := Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	before, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask before resume: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("runs before resume = %d, want 1", len(before))
	}
	firstOutput := before[0].Output
	if got := strings.Count(firstOutput, "[step "); got != 1 {
		t.Fatalf("completed steps before agent error = %d, want 1", got)
	}
	if before[0].Status != domain.RunErrored {
		t.Fatalf("initial run status = %q, want errored", before[0].Status)
	}

	if _, err := Resume(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get task after resume: %v", err)
	}
	if got.Status != domain.TaskDone {
		t.Errorf("task status = %q, want done", got.Status)
	}

	after, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask after resume: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("runs after resume = %d, want 2", len(after))
	}
	if after[0].Output != firstOutput {
		t.Errorf("first run output was modified:\n got: %q\nwant: %q", after[0].Output, firstOutput)
	}
	if after[1].Status != domain.RunSucceeded {
		t.Errorf("resumed run status = %q, want succeeded", after[1].Status)
	}
	if got := strings.Count(after[1].Output, "[step "); got != 3 {
		t.Errorf("steps completed by resumed attempt = %d, want 3", got)
	}
	if strings.Contains(after[1].Output, "[step 1/4]") {
		t.Errorf("resumed output repeated completed step 1: %q", after[1].Output)
	}
}

func TestResumeReportsAnotherTokenExhaustion(t *testing.T) {
	deps, tasks, runs, _ := setupTest(t)

	task := createTask(t, tasks, "very long task", "[steps:30] [budget:1]")
	if err := Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	result, err := Resume(context.Background(), deps, task.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Outcome != domain.AttemptTokenExhausted {
		t.Errorf("result outcome = %q, want token_exhausted", result.Outcome)
	}
	state, err := result.Outcome.TerminalState()
	if err != nil {
		t.Fatalf("TerminalState: %v", err)
	}

	runList, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(runList) != 2 {
		t.Fatalf("runs = %d, want 2", len(runList))
	}
	if runList[1].Status != state.RunStatus || runList[1].ExitReason != state.ExitReason {
		t.Errorf("persisted outcome = (%q, %q), result = (%q, %q)",
			runList[1].Status, runList[1].ExitReason, state.RunStatus, state.ExitReason)
	}
}

type closeOnWrite struct {
	close   func() error
	closeAt int
	writes  int
}

func (w *closeOnWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.closeAt {
		if err := w.close(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func TestExecuteReportsAttemptFinalizationFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tasks := store.NewTaskRepo(db)
	runs := store.NewRunRepo(db)

	task, err := tasks.Create("finalization failure", "[steps:1] [budget:5000]")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	output := &closeOnWrite{
		close:   db.Close,
		closeAt: 2,
	}
	deps := Deps{
		DataDir:  dir,
		Tasks:    tasks,
		Runs:     runs,
		Attempts: store.NewAttemptRepo(db),
		Output:   output,
		Options:  Options{NoDelay: true},
	}

	err = Execute(context.Background(), deps, task.ID)
	if err == nil {
		t.Fatal("Execute returned nil, want attempt finalization error")
	}
	if !strings.Contains(err.Error(), "finish attempt") {
		t.Errorf("Execute error = %q, want attempt finalization context", err)
	}
}

func TestConcurrentResumeAllowsExactlyOneAttempt(t *testing.T) {
	deps, tasks, runs, _ := setupTest(t)

	task := createTask(t, tasks, "contested resume", "[steps:30] [budget:500]")
	if err := Execute(context.Background(), deps, task.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	deps.Options.NoDelay = false
	deps.Output = nil

	type callResult struct {
		attempt AttemptResult
		err     error
	}
	results := make(chan callResult, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)

	for range 2 {
		go func() {
			ready.Done()
			<-start
			attempt, err := Resume(context.Background(), deps, task.ID)
			results <- callResult{attempt: attempt, err: err}
		}()
	}

	ready.Wait()
	close(start)
	first := <-results
	second := <-results

	calls := []callResult{first, second}
	succeeded := 0
	failed := 0
	for _, call := range calls {
		if call.err == nil {
			succeeded++
			continue
		}
		failed++
		if !errors.Is(call.err, domain.ErrConflict) {
			t.Errorf("concurrent loser error = %v, want ErrConflict", call.err)
		}
		var conflict *domain.TaskStatusConflict
		if !errors.As(call.err, &conflict) {
			t.Errorf("concurrent loser error = %v, want TaskStatusConflict", call.err)
			continue
		}
		wantStatus := fmt.Sprintf("status is %q", conflict.Observed)
		if !strings.Contains(call.err.Error(), wantStatus) {
			t.Errorf("concurrent loser error = %q, want observed %s", call.err, wantStatus)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Errorf("resume results: %d succeeded, %d failed; want 1 and 1", succeeded, failed)
	}

	runList, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(runList) != 2 {
		t.Errorf("runs = %d, want 2 (initial attempt plus one resume)", len(runList))
	}
}

func TestExecuteConflict(t *testing.T) {
	deps, tasks, _, _ := setupTest(t)

	task := createTask(t, tasks, "contested", "")
	if err := tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress); err != nil {
		t.Fatalf("transition task: %v", err)
	}

	err := Execute(context.Background(), deps, task.ID)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
}
