package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type OutcomeKind int

const (
	Completed OutcomeKind = iota
	Errored
	TokenBudgetExhausted
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

type SessionState struct {
	ID           string   `json:"id"`
	TaskTitle    string   `json:"task_title"`
	TaskDesc     string   `json:"task_description"`
	Feedback     []string `json:"feedback,omitempty"`
	Steps        []Step   `json:"steps"`
	TokenBudget  int      `json:"token_budget"`
	TokensUsed   int      `json:"tokens_used"`
	NextStep     int      `json:"next_step"`
	Completed    bool     `json:"completed"`
	ErroredAt    int      `json:"errored_at,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
	BudgetHalted bool     `json:"budget_halted"`
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

	budget := 1200
	if v, ok := directives["budget"]; ok {
		budget = v
	}

	steps := buildSteps(h, stepCount, feedback)

	state := SessionState{
		ID:          id,
		TaskTitle:   taskTitle,
		TaskDesc:    taskDescription,
		Feedback:    feedback,
		Steps:       steps,
		TokenBudget: budget,
		TokensUsed:  0,
		NextStep:    0,
	}

	failAt := -1
	if v, ok := directives["fail-at"]; ok {
		failAt = v
		state.ErroredAt = failAt
	}
	_ = failAt

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
	state.TokenBudget = state.TokenBudget - state.TokensUsed + 1200
	state.TokensUsed = 0
	state.BudgetHalted = false

	s := &Session{state: state, dataDir: dataDir}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

func (s *Session) ID() string      { return s.state.ID }
func (s *Session) Budget() int     { return s.state.TokenBudget }
func (s *Session) TokensUsed() int { return s.state.TokensUsed }

func (s *Session) Run(ctx context.Context, onEvent func(Event)) (Outcome, error) {
	failAt := -1
	if s.state.ErroredAt > 0 {
		failAt = s.state.ErroredAt
	}

	for s.state.NextStep < len(s.state.Steps) {
		select {
		case <-ctx.Done():
			s.persist()
			return Outcome{Kind: Errored, Err: ctx.Err()}, nil
		default:
		}

		step := &s.state.Steps[s.state.NextStep]
		stepIdx := s.state.NextStep + 1

		if failAt > 0 && stepIdx == failAt {
			s.state.ErrorMessage = fmt.Sprintf("agent error at step %d: %s", stepIdx, step.Name)
			s.persist()
			return Outcome{Kind: Errored, Err: fmt.Errorf("%s", s.state.ErrorMessage)}, nil
		}

		if s.state.TokensUsed+step.TokenCost > s.state.TokenBudget {
			s.state.BudgetHalted = true
			s.persist()
			return Outcome{Kind: TokenBudgetExhausted}, nil
		}

		if !s.noDelay {
			delay := stepDelay(s.state.ID, s.state.NextStep)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				s.persist()
				return Outcome{Kind: Errored, Err: ctx.Err()}, nil
			case <-timer.C:
			}
		}

		step.Output = fmt.Sprintf("[step %d/%d] %s: processed", stepIdx, len(s.state.Steps), step.Name)
		step.Done = true
		s.state.TokensUsed += step.TokenCost
		s.state.NextStep++

		if onEvent != nil {
			onEvent(Event{
				Step:       stepIdx,
				StepName:   step.Name,
				Output:     step.Output,
				TokensUsed: s.state.TokensUsed,
			})
		}

		if err := s.persist(); err != nil {
			return Outcome{}, fmt.Errorf("persist session: %w", err)
		}
	}

	s.state.Completed = true
	s.persist()

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
	path := filepath.Join(s.dataDir, "sessions", s.state.ID+".json")
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
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
