package store

var migrations = []struct {
	version int
	sql     string
}{
	{
		version: 1,
		sql: `
CREATE TABLE tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	title TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'todo',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id INTEGER NOT NULL REFERENCES tasks(id),
	session_id TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'running',
	exit_reason TEXT NOT NULL DEFAULT '',
	output TEXT NOT NULL DEFAULT '',
	tokens_used INTEGER NOT NULL DEFAULT 0,
	token_budget INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	finished_at TEXT
);
`,
	},
	{
		version: 2,
		sql: `
CREATE TABLE comments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id INTEGER NOT NULL REFERENCES tasks(id),
	author TEXT NOT NULL,
	body TEXT NOT NULL,
	created_at TEXT NOT NULL
);
`,
	},
	{
		version: 3,
		sql: `CREATE INDEX idx_runs_task_id ON runs(task_id);
CREATE INDEX idx_comments_task_id ON comments(task_id);`,
	},
}
