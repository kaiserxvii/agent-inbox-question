package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/cli"
	"github.com/villagelabsco/agent-inbox-question/internal/runner"
)

const usage = `Usage: agent-inbox <command> [flags]

Commands:
  add <title> [-d description]     Create a new task
  list [--status <status>]         List tasks
  show <id>                        Show task details, runs, and output
  run <id>                         Execute a todo task
  resume <id>                      Resume a failed task
  work                             Execute all todo tasks in order
  status                           Show task counts by status
  serve [--reset-interval 30s]     Continue eligible exhausted tasks

Global flags:
  --data-dir <path>    Data directory (default: ~/.agent-inbox)
  --reset-interval     Provider window reset interval (default: 30s)
`

func main() {
	os.Exit(run())
}

func run() int {
	defaultResetInterval, err := resetIntervalFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	dataDir := flag.String("data-dir", "", "data directory")
	resetInterval := flag.Duration(
		"reset-interval",
		defaultResetInterval,
		"provider usage-window reset interval",
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	dir := resolveDataDir(*dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	command := args[0]
	cmdArgs := args[1:]

	app, err := cli.NewApp(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer app.Close()
	app.RunnerOptions.ResetInterval = *resetInterval

	switch command {
	case "add":
		err = app.RunAdd(cmdArgs)
	case "list":
		err = app.RunList(cmdArgs)
	case "show":
		err = app.RunShow(cmdArgs)
	case "run":
		err = app.RunRun(ctx, cmdArgs)
	case "resume":
		err = app.RunResume(ctx, cmdArgs)
	case "work":
		err = app.RunWork(ctx)
	case "status":
		err = app.RunStatus()
	case "serve":
		err = app.RunServe(ctx, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", command)
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "interrupted")
		return 130
	}
	return 0
}

func resetIntervalFromEnv() (time.Duration, error) {
	value := os.Getenv("AGENT_INBOX_RESET_INTERVAL")
	if value == "" {
		return runner.DefaultResetInterval, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse AGENT_INBOX_RESET_INTERVAL: %w", err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("AGENT_INBOX_RESET_INTERVAL must be positive: %s", value)
	}
	return duration, nil
}

func resolveDataDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if envDir := os.Getenv("AGENT_INBOX_DATA_DIR"); envDir != "" {
		return envDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agent-inbox"
	}
	return filepath.Join(home, ".agent-inbox")
}
