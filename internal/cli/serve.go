package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
	"github.com/villagelabsco/agent-inbox-question/internal/server"
)

func (a *App) RunServe(ctx context.Context, args []string) error {
	defaultResetInterval := a.RunnerOptions.ResetInterval
	if defaultResetInterval <= 0 {
		defaultResetInterval = runner.DefaultResetInterval
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(a.errorOutput())
	resetInterval := flags.Duration(
		"reset-interval",
		defaultResetInterval,
		"provider usage-window reset interval",
	)
	scanInterval := flags.Duration(
		"scan-interval",
		time.Second,
		"maximum delay before rescanning durable work",
	)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse serve flags: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("usage: agent-inbox serve [--reset-interval duration] [--scan-interval duration]")
	}

	logger := slog.New(slog.NewJSONHandler(a.errorOutput(), nil))
	srv, err := server.New(
		server.Config{
			ResetInterval: *resetInterval,
			ScanInterval:  *scanInterval,
		},
		server.Dependencies{
			DataDir:       a.DataDir,
			Tasks:         a.Tasks,
			Attempts:      a.Attempts,
			Continuations: a.Continuations,
			Logger:        logger,
			RunnerOptions: a.RunnerOptions,
		},
	)
	if err != nil {
		return fmt.Errorf("configure server: %w", err)
	}
	logger.Info(
		"server started",
		"reset_interval", resetInterval.String(),
		"scan_interval", scanInterval.String(),
	)
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	logger.Info("server stopped")
	return nil
}
