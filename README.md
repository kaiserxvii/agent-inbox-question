# agent-inbox

A task inbox for AI agents. You add tasks; a runner executes them one at a time
by driving a simulated agent; you review the output. Every task terminates in
`done` or `failed`.

There is no real LLM — the agent is simulated deterministically so the tool is
fully self-contained, reproducible, and needs no credentials.

## Getting started

Requirements: Go 1.22 or newer. There are no other dependencies — no API keys,
no services, no database to set up (SQLite is embedded via a pure-Go driver).

```bash
git clone https://github.com/villagelabsco/agent-inbox-question.git
cd agent-inbox-question
make build          # builds ./bin/agent-inbox
make test           # go test ./...
```

Then run your first task:

```bash
./bin/agent-inbox add "my first task" -d "try the thing out"
./bin/agent-inbox work
./bin/agent-inbox show 1
```

State lives in `~/.agent-inbox` by default. To keep experiments isolated, point
the tool at a throwaway directory:

```bash
export AGENT_INBOX_DATA_DIR=$(mktemp -d)
```

## Usage

```
agent-inbox <command> [flags]

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
                       Also settable via AGENT_INBOX_DATA_DIR env var
```

## Worked example

```bash
# Add a normal task
$ agent-inbox add "implement user auth" -d "add JWT-based authentication"
1

# Add a task that will fail at step 2
$ agent-inbox add "broken feature" -d "attempt this [fail-at:2]"
2

# Add a task that will exhaust its token budget
$ agent-inbox add "huge refactor" -d "rewrite everything [steps:30]"
3

# Work through all tasks
$ agent-inbox work
>>> Running task #1: implement user auth
[step 1/6] analyze: processed
[step 2/6] research: processed
...
<<< Task #1: done
>>> Running task #2: broken feature
[step 1/5] analyze: processed
<<< Task #2: failed
>>> Running task #3: huge refactor
[step 1/20] analyze step 1: processed
[step 2/20] research step 2: processed
<<< Task #3: failed

Work complete: 3 tasks processed, 1 succeeded, 2 failed

# Review a completed task
$ agent-inbox show 1

# Check status summary
$ agent-inbox status
todo         0
in_progress  0
done         1
failed       2
total        3

# Resume the task that hit an agent error. Completed work is not repeated.
$ agent-inbox resume 2
[step 2/5] research: processed
...
Task #2: done (completed)

# See both attempts and the output produced by each one.
$ agent-inbox show 2
```

## Directives

Directives can be embedded in the task description to control the simulated
agent's behavior:

| Directive     | Effect                                              |
|---------------|-----------------------------------------------------|
| `[steps:N]`   | Force the agent plan to have exactly N steps         |
| `[fail-at:N]` | The agent errors once at step N; resume retries it   |
| `[budget:N]`  | Set each attempt's token budget (default: 3500)      |

Directives are composable: `[steps:20] [budget:500]` creates a 20-step task
that will exhaust its budget partway through.

## Data directory layout

```
~/.agent-inbox/
  inbox.db           SQLite database (tasks, runs, comments)
  sessions/
    <session-id>.json   Persisted agent session state shared across attempts
```

Override with `--data-dir` or `AGENT_INBOX_DATA_DIR`.

## Design notes

### Package map

```
cmd/agent-inbox/main.go   CLI entry point, flag parsing, signal handling
internal/domain/           Types, statuses, transitions, terminal outcomes
internal/store/            SQLite persistence (modernc.org/sqlite), migrations
internal/agent/            Simulated agent: sessions, steps, token budget
internal/runner/           Prepares sessions, drives attempts, records outcomes
internal/cli/              One file per command, table rendering
```

### Dependencies

The only external dependency is `modernc.org/sqlite` (pure-Go SQLite). No cobra,
no uuid lib, no test framework — standard library only beyond the database driver.

### Resume model

A task and an agent session span one or more runs. A run is a single attempt.
`resume` snapshots the task status and latest terminal run in one database
statement, loads that run's session without mutating it, explicitly begins a new
budget window, then atomically claims the `failed` task and inserts the new
`running` run before driving the remaining steps. A running run is never accepted
as a resume predecessor. Earlier runs are append-only, and `show` groups each run
with the output produced by that attempt.

Attempt start is one SQLite transaction: it compare-and-swaps the task to
`in_progress` and inserts the owning run. If either write fails, neither is kept.
If two callers try to resume the same task, exactly one claims it; the other gets a
typed conflict containing the status observed by the transaction. Resume also binds
the claim to the terminal run captured in its status snapshot, so a fast retry
cannot consume the failure produced by another concurrent invocation. The same
attempt operation is used for initial execution and resume.

At the end of an attempt, the run outcome, task transition, and optional success
comment are committed in one transaction. Callers supply one terminal attempt
outcome; the run status, exit reason, and task status are derived together by the
domain model, so inconsistent combinations cannot be supplied.

Loading a session is pure: it preserves configured allowance, remaining allowance,
attempt usage, and halt state exactly as persisted. Manual resume then explicitly
starts a fresh window equal to the task's configured `[budget:N]`; both agent-error
and token-exhaustion retries currently use that policy. A simulated `[fail-at:N]`
error is transient, so resume retries the unfinished step. A resumed attempt may
still end in `failed` (for example, by exhausting the configured allowance again);
that outcome is recorded and printed explicitly. As with `run`, a completed attempt
that records task failure does not make the command itself fail. Rejected or
conflicting invocations do return a non-zero exit status.

## Known limitations

- Token allowance refresh is immediate and manually triggered by `resume`; there is
  no reset schedule or automatic continuation process yet.
- An interrupted process can leave a task `in_progress`, but its owning `running`
  run is already durable. Recovery of that attempt is not part of Part 1.
- Only one task can be in progress at a time; there is no concurrent execution.
- The simulated agent is deterministic — the same task title and description
  always produce the same plan and step costs.
