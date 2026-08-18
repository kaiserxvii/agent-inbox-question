package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
)

func TestDeterministicPlan(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	s1, err := Start(dir, "test task", "a description", nil, WithNoDelay())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	s2, err := Start(dir, "test task", "a description", nil, WithNoDelay())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(s1.state.Steps) != len(s2.state.Steps) {
		t.Fatalf("step count mismatch: %d vs %d", len(s1.state.Steps), len(s2.state.Steps))
	}
	for i := range s1.state.Steps {
		if s1.state.Steps[i].Name != s2.state.Steps[i].Name {
			t.Errorf("step %d name: %q vs %q", i, s1.state.Steps[i].Name, s2.state.Steps[i].Name)
		}
		if s1.state.Steps[i].TokenCost != s2.state.Steps[i].TokenCost {
			t.Errorf("step %d cost: %d vs %d", i, s1.state.Steps[i].TokenCost, s2.state.Steps[i].TokenCost)
		}
	}
}

func TestDirectiveParsing(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	s, err := Start(dir, "t", "do thing [steps:3] [budget:500]", nil, WithNoDelay())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(s.state.Steps) != 3 {
		t.Errorf("steps = %d, want 3", len(s.state.Steps))
	}
	if s.ConfiguredBudget() != 500 {
		t.Errorf("configured budget = %d, want 500", s.ConfiguredBudget())
	}
}

func TestSuccessfulRun(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	s, _ := Start(dir, "t", "[steps:3] [budget:5000]", nil, WithNoDelay())

	var events []Event
	outcome, err := s.Run(context.Background(), func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != Completed {
		t.Fatalf("outcome = %d, want Completed", outcome.Kind)
	}
	if len(events) != 4 {
		t.Errorf("events = %d, want 4 (3 steps + summary)", len(events))
	}
}

func TestTokenExhaustion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	s, _ := Start(dir, "t", "[steps:10] [budget:1]", nil, WithNoDelay())

	outcome, err := s.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != TokenBudgetExhausted {
		t.Fatalf("outcome = %d, want TokenBudgetExhausted", outcome.Kind)
	}
	if s.state.NextStep != 0 {
		t.Errorf("next_step = %d, want 0 (halted before first step)", s.state.NextStep)
	}

	data, err := os.ReadFile(filepath.Join(dir, "sessions", s.ID()+".json"))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("session file is empty")
	}
}

func TestFailAt(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	s, _ := Start(dir, "t", "[steps:5] [fail-at:2] [budget:5000]", nil, WithNoDelay())

	var events []Event
	outcome, err := s.Run(context.Background(), func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Kind != Errored {
		t.Fatalf("outcome = %d, want Errored", outcome.Kind)
	}
	if outcome.Err == nil {
		t.Fatal("expected error")
	}
	if len(events) != 1 {
		t.Errorf("events = %d, want 1 (one step before fail)", len(events))
	}
}

func TestRunReportsFailureWhenAgentErrorStateCannotBePersisted(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	s, err := Start(
		dir,
		"t",
		"[steps:3] [fail-at:1] [budget:5000]",
		nil,
		WithNoDelay(),
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	sessionPath := filepath.Join(dir, "sessions", s.ID()+".json")
	if err := os.Remove(sessionPath); err != nil {
		t.Fatalf("Remove session file: %v", err)
	}
	if err := os.Mkdir(sessionPath, 0o755); err != nil {
		t.Fatalf("replace session file with directory: %v", err)
	}

	_, err = s.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("Run returned nil, want persistence error")
	}
	if !strings.Contains(err.Error(), "persist agent error") {
		t.Errorf("Run error = %q, want agent error persistence context", err)
	}
}

func TestRunDoesNotPublishStepBeforeItIsPersisted(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	s, err := Start(
		dir,
		"t",
		"[steps:1] [budget:5000]",
		nil,
		WithNoDelay(),
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	sessionPath := filepath.Join(dir, "sessions", s.ID()+".json")
	if err := os.Remove(sessionPath); err != nil {
		t.Fatalf("Remove session file: %v", err)
	}
	if err := os.Mkdir(sessionPath, 0o755); err != nil {
		t.Fatalf("replace session file with directory: %v", err)
	}

	var events []Event
	_, err = s.Run(context.Background(), func(event Event) {
		events = append(events, event)
	})
	if err == nil {
		t.Fatal("Run returned nil, want persistence error")
	}
	if len(events) != 0 {
		t.Errorf("published %d events for undurable work, want 0", len(events))
	}
}

func TestLoadAndContinue(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	s, _ := Start(dir, "t", "[steps:10] [budget:300]", nil, WithNoDelay())

	outcome, _ := s.Run(context.Background(), nil)
	if outcome.Kind != TokenBudgetExhausted {
		t.Fatalf("first run: outcome = %d, want TokenBudgetExhausted", outcome.Kind)
	}
	completedBefore := 0
	for _, step := range s.state.Steps {
		if step.Done {
			completedBefore++
		}
	}
	if completedBefore == 0 {
		t.Skip("no steps completed before exhaustion (budget too low for any step)")
	}

	loaded, err := Load(dir, s.ID(), WithNoDelay())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.state.NextStep != completedBefore {
		t.Errorf("loaded next_step = %d, want %d", loaded.state.NextStep, completedBefore)
	}

	outcome2, err := loaded.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	completedAfter := 0
	for _, step := range loaded.state.Steps {
		if step.Done {
			completedAfter++
		}
	}
	if completedAfter <= completedBefore {
		if outcome2.Kind != TokenBudgetExhausted {
			t.Errorf("expected more steps completed or another exhaustion, got outcome %d", outcome2.Kind)
		}
	}
}

func TestLoadPreservesAttemptBudgetState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	session, err := Start(dir, "t", "[steps:1] [budget:1]", nil, WithNoDelay())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := session.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantBudget := session.AttemptAllowance()
	wantRemaining := session.RemainingBudget()
	wantTokensUsed := session.TokensUsed()
	wantSummary := Summary(session)

	loaded, err := Load(dir, session.ID(), WithNoDelay())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.AttemptAllowance() != wantBudget {
		t.Errorf("loaded allowance = %d, want preserved allowance %d", loaded.AttemptAllowance(), wantBudget)
	}
	if loaded.TokensUsed() != wantTokensUsed {
		t.Errorf("loaded tokens used = %d, want preserved usage %d", loaded.TokensUsed(), wantTokensUsed)
	}
	if loaded.RemainingBudget() != wantRemaining {
		t.Errorf("loaded remaining budget = %d, want %d", loaded.RemainingBudget(), wantRemaining)
	}
	if got := Summary(loaded); got != wantSummary {
		t.Errorf("loaded summary = %q, want preserved state summary %q", got, wantSummary)
	}
}

func TestBeginAttemptUsesExplicitAllowanceWithoutChangingConfiguredBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	session, err := Start(dir, "t", "[steps:10] [budget:400]", nil, WithNoDelay())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := session.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if session.TokensUsed() == 0 {
		t.Fatal("fixture used no tokens before exhaustion")
	}

	if err := session.BeginAttempt(250); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if session.ConfiguredBudget() != 400 {
		t.Errorf("configured budget = %d, want 400", session.ConfiguredBudget())
	}
	if session.AttemptAllowance() != 250 {
		t.Errorf("attempt allowance = %d, want 250", session.AttemptAllowance())
	}
	if session.RemainingBudget() != 250 {
		t.Errorf("remaining budget = %d, want 250", session.RemainingBudget())
	}
	if session.TokensUsed() != 0 {
		t.Errorf("attempt usage = %d, want 0", session.TokensUsed())
	}
}

func TestRunFencedDoesNotExecuteAfterOwnershipIsLost(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatalf("create sessions directory: %v", err)
	}
	session, err := Start(
		dir,
		"stale executor",
		"[steps:1] [budget:5000]",
		nil,
		WithNoDelay(),
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	executed := 0
	_, err = session.RunFenced(
		context.Background(),
		func() error { return domain.ErrLeaseLost },
		func(Event) error {
			executed++
			return nil
		},
	)
	if !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("RunFenced error = %v, want ErrLeaseLost", err)
	}
	if executed != 0 || session.CompletedSteps() != 0 {
		t.Errorf("executed events = %d, completed steps = %d; want zero", executed, session.CompletedSteps())
	}
	loaded, err := Load(dir, session.ID(), WithNoDelay())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CompletedSteps() != 0 {
		t.Errorf("persisted completed steps = %d, want zero", loaded.CompletedSteps())
	}
}

func TestFeedbackSteps(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	feedback := []string{"fix the header", "add tests"}
	s, _ := Start(dir, "t", "[steps:2] [budget:5000]", feedback, WithNoDelay())

	if len(s.state.Steps) != 4 {
		t.Fatalf("steps = %d, want 4 (2 feedback + 2 regular)", len(s.state.Steps))
	}
	if s.state.Steps[0].Name != `revise per feedback: "fix the header"` {
		t.Errorf("first step = %q", s.state.Steps[0].Name)
	}
}

func TestContextCancellation(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s, _ := Start(dir, "t", "[steps:5] [budget:5000]", nil, WithNoDelay())
	outcome, _ := s.Run(ctx, nil)
	if outcome.Kind != Interrupted {
		t.Errorf("outcome = %d, want Interrupted on cancelled context", outcome.Kind)
	}
}

func TestStepCountHelper(t *testing.T) {
	count := StepCount("task", "[steps:7]")
	if count != 7 {
		t.Errorf("StepCount = %d, want 7", count)
	}
	count2 := StepCount("task", "plain desc")
	if count2 < 4 || count2 > 8 {
		t.Errorf("StepCount = %d, want 4-8", count2)
	}
}
