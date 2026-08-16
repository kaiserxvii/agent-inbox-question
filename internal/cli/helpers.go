package cli

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/villagelabsco/agent-inbox-question/internal/store"
)

type App struct {
	DataDir  string
	DB       *store.DB
	Tasks    *store.TaskRepo
	Runs     *store.RunRepo
	Comments *store.CommentRepo
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
		Comments: store.NewCommentRepo(db),
	}, nil
}

func (a *App) Close() error {
	return a.DB.Close()
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

func printTable(headers []string, rows [][]string) {
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
	fmt.Println(header.String())
	fmt.Println(sep.String())

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
		fmt.Println(line.String())
	}
}
