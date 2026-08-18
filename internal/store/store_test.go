package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func completionForTest(
	t *testing.T,
	outcome domain.AttemptOutcome,
	finishedAt time.Time,
	resetInterval time.Duration,
	progressed bool,
) domain.AttemptCompletion {
	t.Helper()
	completion, err := domain.DecideAttemptCompletion(
		outcome,
		finishedAt,
		resetInterval,
		progressed,
	)
	if err != nil {
		t.Fatalf("DecideAttemptCompletion: %v", err)
	}
	return completion
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

func TestMigrationRollsBackDDLWhenVersionCannotBeRecorded(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "inbox.db")
	seedVersionThreeDatabase(t, dbPath)

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seeded database: %v", err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER reject_migration_four_version
		BEFORE INSERT ON schema_migrations
		WHEN NEW.version = 4
		BEGIN
			SELECT RAISE(ABORT, 'version record rejected');
		END`); err != nil {
		raw.Close()
		t.Fatalf("create version rejection trigger: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close seeded database: %v", err)
	}

	if db, err := Open(dir); err == nil {
		db.Close()
		t.Fatal("Open succeeded despite injected migration failure")
	}

	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen seeded database: %v", err)
	}
	defer raw.Close()
	rows, err := raw.Query("PRAGMA table_info(tasks)")
	if err != nil {
		t.Fatalf("read task columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan task column: %v", err)
		}
		if name == "next_eligible_at" || name == "auto_retry_state" || name == "auto_retry_reason" {
			t.Errorf("partially applied column %q survived failed migration", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate task columns: %v", err)
	}
}

func TestConcurrentOpenSerializesMigrations(t *testing.T) {
	dir := t.TempDir()
	seedVersionThreeDatabase(t, filepath.Join(dir, "inbox.db"))

	start := make(chan struct{})
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			db, err := Open(dir)
			if err == nil {
				err = db.Close()
			}
			errors <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open migrated database: %v", err)
	}
	defer db.Close()
	if _, err := NewTaskRepo(db).Create("restart-safe", ""); err != nil {
		t.Fatalf("use migrated database: %v", err)
	}
}

func seedVersionThreeDatabase(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create seed migration table: %v", err)
	}
	for _, migration := range migrations[:3] {
		if _, err := raw.Exec(migration.sql); err != nil {
			t.Fatalf("apply seed migration %d: %v", migration.version, err)
		}
		if _, err := raw.Exec(
			"INSERT INTO schema_migrations (version, applied_at) VALUES (?, datetime('now'))",
			migration.version,
		); err != nil {
			t.Fatalf("record seed migration %d: %v", migration.version, err)
		}
	}
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

	if err := attempts.UpdateProgress(AttemptProgress{
		RunID:      run.ID,
		OwnerToken: run.OwnerToken,
		ObservedAt: time.Now().UTC(),
		Output:     "partial output",
		TokensUsed: 300,
	}); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}

	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID:      run.ID,
		TaskID:     task.ID,
		OwnerToken: run.OwnerToken,
		Completion: completionForTest(t, domain.AttemptCompleted, time.Time{}, 0, false),
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
		RunID: first.ID, TaskID: task.ID, OwnerToken: first.OwnerToken, Completion: completionForTest(t, domain.AttemptTokenExhausted, time.Time{}, 0, false),
	}); err != nil {
		t.Fatalf("first FinishAttempt: %v", err)
	}
	second, err := attempts.StartAttempt(task.ID, domain.TaskFailed, first.ID, "session", 1)
	if err != nil {
		t.Fatalf("second StartAttempt: %v", err)
	}
	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID: second.ID, TaskID: task.ID, OwnerToken: second.OwnerToken, Completion: completionForTest(t, domain.AttemptTokenExhausted, time.Time{}, 0, false),
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

func TestStartAttemptAtomicallyRejectsResumeBeforeProviderReset(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("waiting", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	first, err := attempts.StartOwnedAttempt(StartAttemptParams{
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskTodo,
		SessionID:      "session",
		TokenBudget:    1,
		StartedAt:      now,
		LeaseDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("StartOwnedAttempt: %v", err)
	}
	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID:          first.ID,
		TaskID:         task.ID,
		OwnerToken:     first.OwnerToken,
		Completion:     completionForTest(t, domain.AttemptTokenExhausted, now, time.Hour, true),
		FinishedAt:     now,
		LeaseCheckedAt: now,
	}); err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}

	_, err = attempts.StartOwnedAttempt(StartAttemptParams{
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskFailed,
		ExpectedRunID:  first.ID,
		SessionID:      "session",
		TokenBudget:    1,
		StartedAt:      now.Add(30 * time.Minute),
		LeaseDuration:  time.Minute,
	})
	if !errors.Is(err, domain.ErrNotEligible) {
		t.Fatalf("StartOwnedAttempt error = %v, want ErrNotEligible", err)
	}
	if count, err := runs.CountByTask(task.ID); err != nil {
		t.Fatalf("CountByTask: %v", err)
	} else if count != 1 {
		t.Errorf("runs = %d, want 1", count)
	}
}

func TestResumeCandidateSnapshotRejectsAConcurrentRefailure(t *testing.T) {
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
		RunID: first.ID, TaskID: task.ID, OwnerToken: first.OwnerToken, Completion: completionForTest(t, domain.AttemptTokenExhausted, time.Time{}, 0, false),
	}); err != nil {
		t.Fatalf("first FinishAttempt: %v", err)
	}

	type snapshotResult struct {
		status    domain.TaskStatus
		runID     int64
		sessionID string
		exit      domain.ExitReason
		err       error
	}
	snapshotReady := make(chan snapshotResult, 1)
	releaseStaleCaller := make(chan struct{})
	staleResult := make(chan error, 1)
	go func() {
		candidate, err := attempts.GetResumeCandidate(task.ID)
		if err != nil {
			snapshotReady <- snapshotResult{err: err}
			return
		}
		snapshotReady <- snapshotResult{
			status:    candidate.TaskStatus,
			runID:     candidate.RunID,
			sessionID: candidate.SessionID,
			exit:      candidate.ExitReason,
		}
		<-releaseStaleCaller
		_, err = attempts.StartAttempt(
			task.ID,
			domain.TaskFailed,
			candidate.RunID,
			candidate.SessionID,
			1,
		)
		staleResult <- err
	}()

	captured := <-snapshotReady
	if captured.err != nil {
		t.Fatalf("GetResumeCandidate: %v", captured.err)
	}
	if captured.status != domain.TaskFailed || captured.runID != first.ID ||
		captured.sessionID != "session" || captured.exit != domain.ExitTokenBudgetExhausted {
		t.Fatalf(
			"captured candidate = (%q, %d, %q, %q), want failed run %d",
			captured.status,
			captured.runID,
			captured.sessionID,
			captured.exit,
			first.ID,
		)
	}

	second, err := attempts.StartAttempt(task.ID, domain.TaskFailed, first.ID, "session", 1)
	if err != nil {
		t.Fatalf("concurrent StartAttempt: %v", err)
	}
	if err := attempts.FinishAttempt(FinishAttemptParams{
		RunID: second.ID, TaskID: task.ID, OwnerToken: second.OwnerToken, Completion: completionForTest(t, domain.AttemptTokenExhausted, time.Time{}, 0, false),
	}); err != nil {
		t.Fatalf("concurrent FinishAttempt: %v", err)
	}
	close(releaseStaleCaller)

	err = <-staleResult
	var conflict *domain.TaskStatusConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("stale caller error = %v, want TaskStatusConflict", err)
	}
	if conflict.ExpectedRunID != first.ID || conflict.ObservedRunID != second.ID {
		t.Errorf(
			"conflicting runs = expected %d, observed %d; want %d and %d",
			conflict.ExpectedRunID,
			conflict.ObservedRunID,
			first.ID,
			second.ID,
		)
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
		OwnerToken:    run.OwnerToken,
		Completion:    completionForTest(t, domain.AttemptCompleted, time.Time{}, 0, false),
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
		RunID: first.ID, TaskID: task.ID, OwnerToken: first.OwnerToken, Completion: completionForTest(t, domain.AttemptAgentError, time.Time{}, 0, false),
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

func TestStaleAttemptOwnerCannotPublishProgress(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("fenced task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	run, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "session", 1000)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}

	err = attempts.UpdateProgress(AttemptProgress{
		RunID:      run.ID,
		OwnerToken: "stale-owner-token",
		ObservedAt: time.Now().UTC(),
		Output:     "should not be stored",
		TokensUsed: 100,
	})
	if !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("UpdateProgress error = %v, want ErrLeaseLost", err)
	}

	stored, err := runs.LatestByTask(task.ID)
	if err != nil {
		t.Fatalf("LatestByTask: %v", err)
	}
	if stored.Output != "" || stored.TokensUsed != 0 {
		t.Errorf("stale progress was stored: output=%q tokens=%d", stored.Output, stored.TokensUsed)
	}
}

func TestStaleAttemptCannotRewindSuccessorCheckpoint(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("takeover", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	first, err := attempts.StartOwnedAttempt(StartAttemptParams{
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskTodo,
		SessionID:      "session",
		TokenBudget:    1000,
		StartedAt:      now,
		LeaseDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("start first attempt: %v", err)
	}
	if err := attempts.UpdateProgress(AttemptProgress{
		RunID:      first.ID,
		OwnerToken: first.OwnerToken,
		ObservedAt: now.Add(30 * time.Second),
		Checkpoint: `{"next_step":1}`,
	}); err != nil {
		t.Fatalf("publish first checkpoint: %v", err)
	}
	takeoverAt := now.Add(time.Minute)
	if err := attempts.RecoverExpiredAttempt(FinishAttemptParams{
		RunID:          first.ID,
		TaskID:         task.ID,
		Completion:     completionForTest(t, domain.AttemptInterrupted, time.Time{}, 0, false),
		FinishedAt:     takeoverAt,
		LeaseCheckedAt: takeoverAt,
	}); err != nil {
		t.Fatalf("recover first attempt: %v", err)
	}
	second, err := attempts.StartOwnedAttempt(StartAttemptParams{
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskFailed,
		ExpectedRunID:  first.ID,
		SessionID:      "session",
		TokenBudget:    1000,
		StartedAt:      takeoverAt,
		LeaseDuration:  time.Minute,
	})
	if err != nil {
		t.Fatalf("start successor attempt: %v", err)
	}
	const activeCheckpoint = `{"next_step":2}`
	if err := attempts.UpdateProgress(AttemptProgress{
		RunID:      second.ID,
		OwnerToken: second.OwnerToken,
		ObservedAt: takeoverAt.Add(time.Second),
		Checkpoint: activeCheckpoint,
	}); err != nil {
		t.Fatalf("publish successor checkpoint: %v", err)
	}

	err = attempts.UpdateProgress(AttemptProgress{
		RunID:      first.ID,
		OwnerToken: first.OwnerToken,
		ObservedAt: takeoverAt.Add(time.Second),
		Checkpoint: `{"next_step":1}`,
	})
	if !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale checkpoint error = %v, want ErrLeaseLost", err)
	}
	active, err := runs.LatestByTask(task.ID)
	if err != nil {
		t.Fatalf("LatestByTask: %v", err)
	}
	if active.SessionCheckpoint != activeCheckpoint {
		t.Errorf("active checkpoint = %q, want %q", active.SessionCheckpoint, activeCheckpoint)
	}
}

func TestStaleAttemptOwnerCannotFinalizeRun(t *testing.T) {
	db := openTestDB(t)
	tasks := NewTaskRepo(db)
	attempts := NewAttemptRepo(db)
	runs := NewRunRepo(db)

	task, err := tasks.Create("fenced finalization", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	run, err := attempts.StartAttempt(task.ID, domain.TaskTodo, 0, "session", 1000)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	err = attempts.FinishAttempt(FinishAttemptParams{
		RunID:      run.ID,
		TaskID:     task.ID,
		OwnerToken: "stale-owner-token",
		Completion: completionForTest(t, domain.AttemptCompleted, time.Time{}, 0, false),
		FinishedAt: time.Now().UTC(),
	})
	if !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("FinishAttempt error = %v, want ErrLeaseLost", err)
	}

	stored, err := runs.LatestByTask(task.ID)
	if err != nil {
		t.Fatalf("LatestByTask: %v", err)
	}
	if stored.Status != domain.RunRunning {
		t.Errorf("run status = %q, want running", stored.Status)
	}
	storedTask, err := tasks.Get(task.ID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if storedTask.Status != domain.TaskInProgress {
		t.Errorf("task status = %q, want in_progress", storedTask.Status)
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
