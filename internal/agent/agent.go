package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultTokenBudget covers the worst-case default plan (8 steps at 400 tokens
// plus one feedback step at 300), so a task without directives always finishes.
const DefaultTokenBudget = 3500

type OutcomeKind int

const (
	OutcomeUnknown OutcomeKind = iota
	Completed
	Errored
	TokenBudgetExhausted
	Interrupted
)

type Outcome struct {
	Kind OutcomeKind
	Err  error
}

type Event struct {
	Step       int
	StepName   string
	Output     string
	TokensUsed int
}

type Step struct {
	Name      string `json:"name"`
	TokenCost int    `json:"token_cost"`
	Done      bool   `json:"done"`
	Output    string `json:"output,omitempty"`
}

type HaltReason string

const (
	HaltRunning        HaltReason = "running"
	HaltCompleted      HaltReason = "completed"
	HaltAgentError     HaltReason = "agent_error"
	HaltTokenExhausted HaltReason = "token_exhausted"
	HaltInterrupted    HaltReason = "interrupted"
)

type AttemptCheckpoint struct {
	RunID           int64
	OwnerToken      string
	StartStep       int
	CompletedSteps  int
	TokensUsed      int
	TokensRemaining int
	HaltReason      HaltReason
	Error           string
	Output          string
}

type SessionState struct {
	ID                    string     `json:"id"`
	TaskTitle             string     `json:"task_title"`
	TaskDesc              string     `json:"task_description"`
	Feedback              []string   `json:"feedback,omitempty"`
	Steps                 []Step     `json:"steps"`
	BudgetModelVersion    int        `json:"budget_model_version"`
	ConfiguredTokenBudget int        `json:"configured_token_budget"`
	AttemptTokenAllowance int        `json:"attempt_token_allowance"`
	TokensRemaining       int        `json:"tokens_remaining"`
	TokensUsed            int        `json:"tokens_used"`
	NextStep              int        `json:"next_step"`
	Completed             bool       `json:"completed"`
	ErroredAt             int        `json:"errored_at,omitempty"`
	ErrorMessage          string     `json:"error_message,omitempty"`
	BudgetHalted          bool       `json:"budget_halted"`
	LegacyTokenBudget     int        `json:"token_budget,omitempty"`
	AttemptRunID          int64      `json:"attempt_run_id,omitempty"`
	AttemptOwnerToken     string     `json:"attempt_owner_token,omitempty"`
	AttemptStartStep      int        `json:"attempt_start_step"`
	AttemptHaltReason     HaltReason `json:"attempt_halt_reason,omitempty"`
}

type Session struct {
	state   SessionState
	dataDir string
	noDelay bool
}

type Option func(*Session)

func WithNoDelay() Option {
	return func(s *Session) {
		s.noDelay = true
	}
}

func Start(dataDir, taskTitle, taskDescription string, feedback []string, opts ...Option) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}

	directives := parseDirectives(taskDescription)
	h := hashInput(taskTitle, taskDescription)

	stepCount := boundedHash(h, 0, 4, 8)
	if v, ok := directives["steps"]; ok {
		stepCount = v
	}

	budget := DefaultTokenBudget
	if v, ok := directives["budget"]; ok {
		budget = v
	}

	steps := buildSteps(h, stepCount, feedback)

	state := SessionState{
		ID:                    id,
		TaskTitle:             taskTitle,
		TaskDesc:              taskDescription,
		Feedback:              feedback,
		Steps:                 steps,
		BudgetModelVersion:    2,
		ConfiguredTokenBudget: budget,
		AttemptTokenAllowance: budget,
		TokensRemaining:       budget,
		TokensUsed:            0,
		NextStep:              0,
	}

	if failAt, ok := directives["fail-at"]; ok {
		state.ErroredAt = failAt
	}

	s := &Session{state: state, dataDir: dataDir}
	for _, o := range opts {
		o(s)
	}

	if err := s.persist(); err != nil {
		return nil, err
	}
	return s, nil
}

func Load(dataDir, sessionID string, opts ...Option) (*Session, error) {
	path := filepath.Join(dataDir, "sessions", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", sessionID, err)
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", sessionID, err)
	}
	if state.BudgetModelVersion < 2 {
		state.BudgetModelVersion = 2
		if state.ConfiguredTokenBudget == 0 {
			state.ConfiguredTokenBudget = state.LegacyTokenBudget
		}
		state.AttemptTokenAllowance = state.LegacyTokenBudget
		if state.TokensRemaining == 0 && state.LegacyTokenBudget > 0 {
			state.TokensRemaining = max(0, state.LegacyTokenBudget-state.TokensUsed)
		}
		state.LegacyTokenBudget = 0
	}
	s := &Session{state: state, dataDir: dataDir}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

func (s *Session) ID() string            { return s.state.ID }
func (s *Session) ConfiguredBudget() int { return s.state.ConfiguredTokenBudget }
func (s *Session) AttemptAllowance() int { return s.state.AttemptTokenAllowance }
func (s *Session) RemainingBudget() int  { return s.state.TokensRemaining }
func (s *Session) TokensUsed() int       { return s.state.TokensUsed }
func (s *Session) CompletedSteps() int   { return s.state.NextStep }

// BeginAttempt applies an explicit provider-window allowance to the next
// attempt. It only changes in-memory attempt state; Run persists that state as
// work progresses.
func (s *Session) BeginAttempt(allowance int) error {
	if allowance < 0 {
		return fmt.Errorf("attempt allowance must be non-negative: %d", allowance)
	}
	s.state.AttemptTokenAllowance = allowance
	s.state.TokensRemaining = allowance
	s.state.TokensUsed = 0
	s.state.BudgetHalted = false
	return nil
}

func (s *Session) ContinueAttempt() {
	s.state.AttemptTokenAllowance = s.state.TokensRemaining
	s.state.TokensUsed = 0
	s.state.BudgetHalted = false
}

func (s *Session) BindAttempt(runID int64, ownerToken string) error {
	s.state.AttemptRunID = runID
	s.state.AttemptOwnerToken = ownerToken
	s.state.AttemptStartStep = s.state.NextStep
	s.state.AttemptHaltReason = HaltRunning
	s.state.ErrorMessage = ""
	if err := s.persist(); err != nil {
		return fmt.Errorf("persist attempt binding: %w", err)
	}
	return nil
}

func (s *Session) AttemptCheckpoint() AttemptCheckpoint {
	lines := make([]string, 0, s.state.NextStep-s.state.AttemptStartStep+1)
	for index := s.state.AttemptStartStep; index < s.state.NextStep; index++ {
		if s.state.Steps[index].Output != "" {
			lines = append(lines, s.state.Steps[index].Output)
		}
	}
	if s.state.AttemptHaltReason == HaltCompleted {
		lines = append(lines, fmt.Sprintf(
			"completed all %d steps for: %s",
			len(s.state.Steps),
			s.state.TaskTitle,
		))
	}
	return AttemptCheckpoint{
		RunID:           s.state.AttemptRunID,
		OwnerToken:      s.state.AttemptOwnerToken,
		StartStep:       s.state.AttemptStartStep,
		CompletedSteps:  s.state.NextStep,
		TokensUsed:      s.state.TokensUsed,
		TokensRemaining: s.state.TokensRemaining,
		HaltReason:      s.state.AttemptHaltReason,
		Error:           s.state.ErrorMessage,
		Output:          strings.Join(lines, "\n"),
	}
}

func (s *Session) Run(ctx context.Context, onEvent func(Event)) (Outcome, error) {
	failAt := -1
	if s.state.ErroredAt > 0 {
		failAt = s.state.ErroredAt
	}

	for s.state.NextStep < len(s.state.Steps) {
		select {
		case <-ctx.Done():
			s.state.AttemptHaltReason = HaltInterrupted
			if err := s.persist(); err != nil {
				return Outcome{}, fmt.Errorf("persist canceled session: %w", err)
			}
			return Outcome{Kind: Interrupted, Err: ctx.Err()}, nil
		default:
		}

		step := &s.state.Steps[s.state.NextStep]
		stepIdx := s.state.NextStep + 1

		if failAt > 0 && stepIdx == failAt {
			s.state.AttemptHaltReason = HaltAgentError
			s.state.ErrorMessage = fmt.Sprintf("agent error at step %d: %s", stepIdx, step.Name)
			s.state.ErroredAt = 0
			if err := s.persist(); err != nil {
				return Outcome{}, fmt.Errorf("persist agent error: %w", err)
			}
			return Outcome{Kind: Errored, Err: errors.New(s.state.ErrorMessage)}, nil
		}

		if step.TokenCost > s.state.TokensRemaining {
			s.state.BudgetHalted = true
			s.state.AttemptHaltReason = HaltTokenExhausted
			if err := s.persist(); err != nil {
				return Outcome{}, fmt.Errorf("persist token exhaustion: %w", err)
			}
			return Outcome{Kind: TokenBudgetExhausted}, nil
		}

		if !s.noDelay {
			delay := stepDelay(s.state.ID, s.state.NextStep)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				s.state.AttemptHaltReason = HaltInterrupted
				if err := s.persist(); err != nil {
					return Outcome{}, fmt.Errorf("persist canceled session: %w", err)
				}
				return Outcome{Kind: Interrupted, Err: ctx.Err()}, nil
			case <-timer.C:
			}
		}

		step.Output = fmt.Sprintf("[step %d/%d] %s: processed", stepIdx, len(s.state.Steps), step.Name)
		step.Done = true
		s.state.TokensUsed += step.TokenCost
		s.state.TokensRemaining -= step.TokenCost
		s.state.NextStep++

		if err := s.persist(); err != nil {
			return Outcome{}, fmt.Errorf("persist session: %w", err)
		}

		if onEvent != nil {
			onEvent(Event{
				Step:       stepIdx,
				StepName:   step.Name,
				Output:     step.Output,
				TokensUsed: s.state.TokensUsed,
			})
		}
	}

	s.state.Completed = true
	s.state.AttemptHaltReason = HaltCompleted
	if err := s.persist(); err != nil {
		return Outcome{}, fmt.Errorf("persist completed session: %w", err)
	}

	summary := fmt.Sprintf("completed all %d steps for: %s", len(s.state.Steps), s.state.TaskTitle)
	if onEvent != nil {
		onEvent(Event{
			Step:       len(s.state.Steps),
			StepName:   "summary",
			Output:     summary,
			TokensUsed: s.state.TokensUsed,
		})
	}

	return Outcome{Kind: Completed}, nil
}

func (s *Session) persist() error {
	sessionsDir := filepath.Join(s.dataDir, "sessions")
	path := filepath.Join(sessionsDir, s.state.ID+".json")
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	temp, err := os.CreateTemp(sessionsDir, "."+s.state.ID+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func(cause error) error {
		closeErr := temp.Close()
		removeErr := os.Remove(tempPath)
		if closeErr != nil {
			cause = errors.Join(cause, fmt.Errorf("close temporary session file: %w", closeErr))
		}
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cause = errors.Join(cause, fmt.Errorf("remove temporary session file: %w", removeErr))
		}
		return cause
	}
	if err := temp.Chmod(0o644); err != nil {
		return cleanup(fmt.Errorf("set session file permissions: %w", err))
	}
	if _, err := temp.Write(data); err != nil {
		return cleanup(fmt.Errorf("write temporary session file: %w", err))
	}
	if err := temp.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync temporary session file: %w", err))
	}
	if err := temp.Close(); err != nil {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("close temporary session file: %w", err),
				fmt.Errorf("remove temporary session file: %w", removeErr),
			)
		}
		return fmt.Errorf("close temporary session file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("replace session file: %w", err),
				fmt.Errorf("remove temporary session file: %w", removeErr),
			)
		}
		return fmt.Errorf("replace session file: %w", err)
	}
	return nil
}

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashInput(title, description string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(title))
	h.Write([]byte(description))
	return h.Sum32()
}

func boundedHash(h uint32, salt, lo, hi int) int {
	mixed := h ^ uint32(salt*2654435761)
	span := hi - lo + 1
	return lo + int(mixed%uint32(span))
}

var directiveRe = regexp.MustCompile(`\[(steps|fail-at|budget):(\d+)\]`)

func parseDirectives(desc string) map[string]int {
	directives := make(map[string]int)
	for _, m := range directiveRe.FindAllStringSubmatch(desc, -1) {
		v, err := strconv.Atoi(m[2])
		if err == nil {
			directives[m[1]] = v
		}
	}
	return directives
}

var stepNames = []string{
	"analyze", "research", "design", "draft", "implement",
	"refine", "test", "verify", "document", "review",
}

func buildSteps(h uint32, count int, feedback []string) []Step {
	var steps []Step

	for _, fb := range feedback {
		steps = append(steps, Step{
			Name:      fmt.Sprintf("revise per feedback: %q", truncate(fb, 40)),
			TokenCost: boundedHash(h, len(steps)+100, 150, 300),
		})
	}

	for i := 0; i < count; i++ {
		name := stepNames[i%len(stepNames)]
		if count > len(stepNames) {
			name = fmt.Sprintf("%s step %d", name, i+1)
		}
		steps = append(steps, Step{
			Name:      name,
			TokenCost: boundedHash(h, i, 150, 400),
		})
	}

	return steps
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func stepDelay(sessionID string, step int) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	h.Write([]byte(strconv.Itoa(step)))
	ms := 150 + int(h.Sum32()%251)
	return time.Duration(ms) * time.Millisecond
}

func StepCount(title, description string) int {
	directives := parseDirectives(description)
	if v, ok := directives["steps"]; ok {
		return v
	}
	h := hashInput(title, description)
	return boundedHash(h, 0, 4, 8)
}

func Summary(s *Session) string {
	done := 0
	for _, step := range s.state.Steps {
		if step.Done {
			done++
		}
	}
	parts := []string{fmt.Sprintf("%d/%d steps completed", done, len(s.state.Steps))}
	if s.state.Completed {
		parts = append(parts, "status: completed")
	} else if s.state.BudgetHalted {
		parts = append(parts, "status: budget exhausted")
	} else if s.state.ErrorMessage != "" {
		parts = append(parts, "status: errored")
	}

	feedbackSummary := strings.Join(s.state.Feedback, "; ")
	if feedbackSummary != "" {
		parts = append(parts, fmt.Sprintf("feedback addressed: %s", truncate(feedbackSummary, 60)))
	}

	return strings.Join(parts, ", ")
}
