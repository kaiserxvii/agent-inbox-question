package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close database: %v", err)
		}
	})
	return db
}

func TestTaskCRUD(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)

	task, err := tasks.Create("test task", "description here")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	if task.Status != domain.TaskTodo {
		t.Errorf("status = %q, want %q", task.Status, domain.TaskTodo)
	}

	got, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "test task" {
		t.Errorf("title = %q, want %q", got.Title, "test task")
	}
	if got.Description != "description here" {
		t.Errorf("description = %q, want %q", got.Description, "description here")
	}

	_, err = tasks.Get(9999)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(9999) = %v, want ErrNotFound", err)
	}
}

func TestTaskList(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)

	tasks.Create("first", "")
	tasks.Create("second", "")

	list, err := tasks.List(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}

	status := domain.TaskTodo
	filtered, err := tasks.List(&status)
	if err != nil {
		t.Fatalf("List filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered len = %d, want 2", len(filtered))
	}
}

func TestTaskTransition(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)

	task, _ := tasks.Create("t", "")

	if err := tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress); err != nil {
		t.Fatalf("todo->in_progress: %v", err)
	}

	got, _ := tasks.Get(task.ID)
	if got.Status != domain.TaskInProgress {
		t.Errorf("status = %q, want in_progress", got.Status)
	}

	err := tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("double claim = %v, want ErrConflict", err)
	}
}

func TestCASClaimConflict(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)

	task, _ := tasks.Create("contested", "")

	const N = 8
	var mu sync.Mutex
	winners := 0
	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			err := tasks.Transition(task.ID, domain.TaskTodo, domain.TaskInProgress)
			if err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
}

func TestMigrationIdempotency(t *testing.T) {
	dir := t.TempDir()
	db1, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db1.Close()

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
}

func TestAttemptLifecycle(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	run, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "abc123", 1200)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if run.Status != domain.RunRunning {
		t.Errorf("status = %q, want running", run.Status)
	}
	if run.TokenBudget != 1200 {
		t.Errorf("budget = %d, want 1200", run.TokenBudget)
	}

	if err := attempts.UpdateProgress(run.ID, "partial output", 300); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}

	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID:      run.ID,
		TaskID:     task.ID,
		Outcome:    domain.AttemptCompleted,
		Output:     "final output",
		TokensUsed: 800,
	}); err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}

	list, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	r := list[0]
	if r.Status != domain.RunSucceeded {
		t.Errorf("status = %q, want succeeded", r.Status)
	}
	if r.Output != "final output" {
		t.Errorf("output = %q", r.Output)
	}
	if r.FinishedAt == nil {
		t.Error("finished_at should be set")
	}
}

func TestStartAttemptRollsBackClaimWhenRunCannotBeRecorded(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	if _, err := db.sql.Exec(`
		CREATE TRIGGER reject_run_insert
		BEFORE INSERT ON runs
		BEGIN
			SELECT RAISE(ABORT, 'run insert rejected');
		END;
	`); err != nil {
		t.Fatalf("create rejection trigger: %v", err)
	}

	_, err = attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "session", 1000)
	if err == nil {
		t.Fatal("StartAttempt returned nil, want run creation error")
	}

	got, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if got.Status != domain.TaskTodo {
		t.Errorf("task status = %q, want todo after rollback", got.Status)
	}
}

func TestStartAttemptConflictReportsObservedTaskStatus(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	if _, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "winner", 1000); err != nil {
		t.Fatalf("first StartAttempt: %v", err)
	}

	_, err = attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "loser", 1000)
	var conflict *domain.TaskStatusConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("second StartAttempt error = %v, want TaskStatusConflict", err)
	}
	if conflict.Observed != domain.TaskInProgress {
		t.Errorf("observed status = %q, want in_progress", conflict.Observed)
	}
}

func TestStartAttemptRejectsAResumeOfAnAlreadyConsumedRun(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	first, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "session", 1)
	if err != nil {
		t.Fatalf("first StartAttempt: %v", err)
	}
	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID: first.ID, TaskID: task.ID, Outcome: domain.AttemptTokenExhausted,
	}); err != nil {
		t.Fatalf("first FinishAttempt: %v", err)
	}
	second, err := attempts.StartAttempt(task.ID, domain.TaskFailed, first.ID, "session", 1)
	if err != nil {
		t.Fatalf("second StartAttempt: %v", err)
	}
	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID: second.ID, TaskID: task.ID, Outcome: domain.AttemptTokenExhausted,
	}); err != nil {
		t.Fatalf("second FinishAttempt: %v", err)
	}

	_, err = attempts.StartAttempt(task.ID, domain.TaskFailed, first.ID, "session", 1)
	var conflict *domain.TaskStatusConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("stale StartAttempt error = %v, want TaskStatusConflict", err)
	}
	if conflict.Observed != domain.TaskFailed {
		t.Errorf("observed status = %q, want failed", conflict.Observed)
	}
	if conflict.ObservedRunID != second.ID {
		t.Errorf("observed run ID = %d, want %d", conflict.ObservedRunID, second.ID)
	}
	count, err := runs.CountByTask(task.ID)
	if err != nil {
		t.Fatalf("CountByTask: %v", err)
	}
	if count != 2 {
		t.Errorf("runs = %d, want 2", count)
	}
}

func TestFinishAttemptRollsBackWhenTaskCannotTransition(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	run, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "session", 1000)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}

	err = attempts.FinishAttempt(FinishAttemptParams{
		RunID:         run.ID,
		TaskID:        task.ID + 1,
		Outcome:       domain.AttemptCompleted,
		Output:        "completed output",
		TokensUsed:    500,
		CommentAuthor: "agent",
		CommentBody:   "complete",
	})
	if err == nil {
		t.Fatal("FinishAttempt returned nil, want task transition error")
	}

	runList, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(runList) != 1 {
		t.Fatalf("runs = %d, want 1", len(runList))
	}
	if runList[0].Status != domain.RunRunning {
		t.Errorf("run status = %q, want running after rollback", runList[0].Status)
	}
	if runList[0].FinishedAt != nil {
		t.Error("run finished_at was set despite rollback")
	}

	got, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if got.Status != domain.TaskInProgress {
		t.Errorf("task status = %q, want in_progress after rollback", got.Status)
	}
}

func TestFinishAttemptRejectsUnknownOutcome(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	run, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "session", 1000)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}

	err = attempts.FinishAttempt(FinishAttemptParams{
		RunID:   run.ID,
		TaskID:  task.ID,
		Outcome: domain.AttemptOutcome("unknown"),
	})
	if err == nil {
		t.Fatal("FinishAttempt returned nil for unknown outcome")
	}

	runList, err := runs.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if runList[0].Status != domain.RunRunning {
		t.Errorf("run status = %q, want running", runList[0].Status)
	}
	got, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if got.Status != domain.TaskInProgress {
		t.Errorf("task status = %q, want in_progress", got.Status)
	}
}

func TestCommentCRUD(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	comments := NewCommentRepo(db)

	task, _ := tasks.Create("t", "")

	c, err := comments.Create(task.ID, "user", "needs more work")
	if err != nil {
		t.Fatalf("Create comment: %v", err)
	}
	if c.Author != "user" {
		t.Errorf("author = %q", c.Author)
	}

	comments.Create(task.ID, "agent", "done revising")

	list, err := comments.ListByTask(task.ID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
}

func TestCountByStatus(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)

	tasks.Create("a", "")
	tasks.Create("b", "")
	t2, _ := tasks.Create("c", "")
	tasks.Transition(t2.ID, domain.TaskTodo, domain.TaskInProgress)
	tasks.Transition(t2.ID, domain.TaskInProgress, domain.TaskDone)

	counts, err := tasks.CountByStatus()
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[domain.TaskTodo] != 2 {
		t.Errorf("todo = %d, want 2", counts[domain.TaskTodo])
	}
	if counts[domain.TaskDone] != 1 {
		t.Errorf("done = %d, want 1", counts[domain.TaskDone])
	}
}

func TestOldestByStatus(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)

	t1, _ := tasks.Create("first", "")
	tasks.Create("second", "")

	oldest, err := tasks.OldestByStatus(domain.TaskTodo)
	if err != nil {
		t.Fatalf("OldestByStatus: %v", err)
	}
	if oldest.ID != t1.ID {
		t.Errorf("got id %d, want %d", oldest.ID, t1.ID)
	}

	_, err = tasks.OldestByStatus(domain.TaskDone)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound for empty status, got: %v", err)
	}
}

func TestRunCountByTask(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, _ := tasks.Create("t", "")
	first, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "s1", 1000)
	if err != nil {
		t.Fatalf("first StartAttempt: %v", err)
	}
	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID: first.ID, TaskID: task.ID, Outcome: domain.AttemptAgentError,
	}); err != nil {
		t.Fatalf("first FinishAttempt: %v", err)
	}
	if _, err := attempts.StartAttempt(task.ID, domain.TaskFailed, first.ID, "s2", 1000); err != nil {
		t.Fatalf("second StartAttempt: %v", err)
	}

	count, err := runs.CountByTask(task.ID)
	if err != nil {
		t.Fatalf("CountByTask: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestLatestRunByTaskReturnsNewestAttempt(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("t", "")
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}
	first, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "first", 1000)
	if err != nil {
		t.Fatalf("first StartAttempt: %v", err)
	}
	if err := tasks.Transition(task.ID, domain.TaskInProgress, domain.TaskFailed); err != nil {
		t.Fatalf("fail first attempt task: %v", err)
	}
	second, err := attempts.StartAttempt(task.ID, domain.TaskFailed, first.ID, "second", 1000)
	if err != nil {
		t.Fatalf("second StartAttempt: %v", err)
	}

	latest, err := runs.LatestByTask(task.ID)
	if err != nil {
		t.Fatalf("LatestByTask: %v", err)
	}
	if latest.ID != second.ID {
		t.Errorf("latest run ID = %d, want %d", latest.ID, second.ID)
	}
}

func TestSessionsDirCreated(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()

	sessDir := filepath.Join(dir, "sessions")
	info, err := filepath.Glob(sessDir)
	if err != nil || len(info) == 0 {
		t.Error("sessions/ directory not created")
	}
}
