package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/villagelabsco/agent-inbox-question/internal/cli"
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

Global flags:
  --data-dir <path>    Data directory (default: ~/.agent-inbox)
`

func main() {
	os.Exit(run())
}

func run() int {
	dataDir := flag.String("data-dir", "", "data directory")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	dir := resolveDataDir(*dataDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	command := args[0]
	cmdArgs := args[1:]

	app, err := cli.NewApp(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer app.Close()

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
