package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/domain"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

type Config struct {
	ResetInterval time.Duration
	ScanInterval  time.Duration
}

type Dependencies struct {
	DataDir       string
	Tasks         *store.TaskRepo
	Attempts      *store.AttemptRepo
	Continuations *store.ContinuationRepo
	Logger        *slog.Logger
	RunnerOptions runner.Options
}

type clock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now().UTC()
}

func (realClock) Wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Server struct {
	config Config
	deps   Dependencies
	clock  clock
}

func New(config Config, deps Dependencies) (*Server, error) {
	if config.ResetInterval <= 0 {
		return nil, errors.New("reset interval must be positive")
	}
	if config.ScanInterval <= 0 {
		return nil, errors.New("scan interval must be positive")
	}
	if deps.Tasks == nil || deps.Attempts == nil || deps.Continuations == nil {
		return nil, errors.New("server dependencies are incomplete")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Server{
		config: config,
		deps:   deps,
		clock:  realClock{},
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		initialized, err := s.deps.Continuations.InitializeUnscheduled(s.config.ResetInterval)
		if err == nil {
			s.deps.Logger.Info(
				"initialized automatic continuation",
				"task_id", initialized.TaskID,
				"eligible_at", initialized.Continuation.EligibleAt(),
				"state", initialized.Continuation.Kind(),
				"reason", initialized.Continuation.Reason(),
			)
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrConflict) {
			return fmt.Errorf("initialize unscheduled continuation: %w", err)
		}

		now := s.clock.Now()
		expired, err := s.deps.Attempts.NextExpired()
		if err == nil {
			if err := runner.RecoverExpired(
				runner.Deps{
					DataDir:  s.deps.DataDir,
					Tasks:    s.deps.Tasks,
					Attempts: s.deps.Attempts,
					Options:  s.deps.RunnerOptions,
				},
				expired,
				s.config.ResetInterval,
				now,
			); err != nil {
				return fmt.Errorf("recover expired run %d: %w", expired.ID, err)
			}
			s.deps.Logger.Info(
				"recovered expired attempt",
				"task_id", expired.TaskID,
				"run_id", expired.ID,
			)
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("find expired attempt: %w", err)
		}

		continuation, err := s.deps.Continuations.Next()
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("find next continuation: %w", err)
			}
			s.deps.Logger.Info("nothing eligible", "next_wake_in", s.config.ScanInterval)
			if err := s.clock.Wait(ctx, s.config.ScanInterval); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("wait to rescan: %w", err)
			}
			continue
		}

		untilEligible := continuation.EligibleAt.Sub(s.clock.Now())
		if untilEligible > 0 {
			wait := min(untilEligible, s.config.ScanInterval)
			s.deps.Logger.Info(
				"waiting for task eligibility",
				"task_id", continuation.TaskID,
				"eligible_at", continuation.EligibleAt,
				"next_wake_in", wait,
			)
			if err := s.clock.Wait(ctx, wait); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("wait for task eligibility: %w", err)
			}
			continue
		}

		options := s.deps.RunnerOptions
		options.Now = s.clock.Now
		options.ResetInterval = s.config.ResetInterval
		result, err := runner.Resume(ctx, runner.Deps{
			DataDir:  s.deps.DataDir,
			Tasks:    s.deps.Tasks,
			Attempts: s.deps.Attempts,
			Options:  options,
		}, continuation.TaskID)
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				continue
			}
			return fmt.Errorf("continue task %d: %w", continuation.TaskID, err)
		}
		task, err := s.deps.Tasks.Get(continuation.TaskID)
		if err != nil {
			return fmt.Errorf("read continued task %d: %w", continuation.TaskID, err)
		}
		s.deps.Logger.Info(
			"continuation attempt finished",
			"task_id", continuation.TaskID,
			"outcome", result.Outcome,
			"task_status", task.Status,
			"auto_retry_state", task.Continuation.Kind(),
			"auto_retry_reason", task.Continuation.Reason(),
			"next_eligible_at", task.Continuation.EligibleAt(),
		)
	}
}
