package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/runner"
	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

type App struct {
	DataDir       string
	DB            *store.DB
	Tasks         *store.TaskRepo
	Runs          *store.RunRepo
	Attempts      *store.AttemptRepo
	Comments      *store.CommentRepo
	Stdout        io.Writer
	Stderr        io.Writer
	RunnerOptions runner.Options
}

func NewApp(dataDir string) (*App, error) {
	db, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	return &App{
		DataDir:  dataDir,
		DB:       db,
		Tasks:    store.NewTaskRepo(db),
		Runs:     store.NewRunRepo(db),
		Attempts: store.NewAttemptRepo(db),
		Comments: store.NewCommentRepo(db),
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	}, nil
}

func (a *App) Close() error {
	return a.DB.Close()
}

func (a *App) output() io.Writer {
	if a.Stdout != nil {
		return a.Stdout
	}
	return os.Stdout
}

func (a *App) errorOutput() io.Writer {
	if a.Stderr != nil {
		return a.Stderr
	}
	return os.Stderr
}

type outputPrinter struct {
	writer io.Writer
	err    error
}

func newOutputPrinter(writer io.Writer) *outputPrinter {
	return &outputPrinter{writer: writer}
}

func (p *outputPrinter) Printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.writer, format, args...)
}

func (p *outputPrinter) Println(args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintln(p.writer, args...)
}

func (p *outputPrinter) Err() error {
	if p.err != nil {
		return fmt.Errorf("write output: %w", p.err)
	}
	return nil
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(math.Floor(d.Hours() / 24))
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func printTable(writer io.Writer, headers []string, rows [][]string) error {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, col := range row {
			if i < len(widths) && len(col) > widths[i] {
				widths[i] = len(col)
			}
		}
	}

	var header strings.Builder
	var sep strings.Builder
	for i, h := range headers {
		if i > 0 {
			header.WriteString("  ")
			sep.WriteString("  ")
		}
		header.WriteString(padRight(h, widths[i]))
		sep.WriteString(strings.Repeat("-", widths[i]))
	}
	out := newOutputPrinter(writer)
	out.Println(header.String())
	out.Println(sep.String())

	for _, row := range rows {
		var line strings.Builder
		for i, col := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			if i < len(widths) {
				line.WriteString(padRight(col, widths[i]))
			} else {
				line.WriteString(col)
			}
		}
		out.Println(line.String())
	}
	return out.Err()
}
