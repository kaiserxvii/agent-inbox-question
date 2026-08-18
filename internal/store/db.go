package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "inbox.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	d := &DB{sql: db}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := d.enableWAL(); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.sql.Close()
}

func (d *DB) migrate() error {
	ctx := context.Background()
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	rollback := func(cause error) error {
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback migrations: %w", rollbackErr))
		}
		return cause
	}

	_, err = conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`)
	if err != nil {
		return rollback(fmt.Errorf("create migrations table: %w", err))
	}

	for _, m := range migrations {
		var count int
		if err := conn.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE version = ?",
			m.version,
		).Scan(&count); err != nil {
			return rollback(fmt.Errorf("check migration %d: %w", m.version, err))
		}
		if count > 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx, m.sql); err != nil {
			return rollback(fmt.Errorf("apply migration %d: %w", m.version, err))
		}
		if _, err := conn.ExecContext(
			ctx,
			"INSERT INTO schema_migrations (version, applied_at) VALUES (?, datetime('now'))",
			m.version,
		); err != nil {
			return rollback(fmt.Errorf("record migration %d: %w", m.version, err))
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(fmt.Errorf("commit migrations: %w", err))
	}
	return nil
}

func (d *DB) enableWAL() error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		err := d.sql.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil {
			if !strings.EqualFold(mode, "wal") {
				return fmt.Errorf("journal mode is %q", mode)
			}
			return nil
		}
		if time.Now().After(deadline) ||
			(!strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy")) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
