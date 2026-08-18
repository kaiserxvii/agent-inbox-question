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
  serve [--reset-interval 30s]     Continue eligible exhausted tasks

Global flags:
  --data-dir <path>       Data directory (default: ~/.agent-inbox)
                          Also settable via AGENT_INBOX_DATA_DIR
  --reset-interval <dur>  Provider window reset interval (default: 30s)
                          Also settable via AGENT_INBOX_RESET_INTERVAL
```

Global flags go before the command. `serve` also accepts `--reset-interval`
after the command, plus `--scan-interval` to control how quickly it notices
work written by another CLI process:

```bash
agent-inbox serve --reset-interval 10s --scan-interval 1s
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

# Automatically continue token-exhausted tasks as their windows reset.
# Logs are structured JSON and the process stops cleanly on SIGINT/SIGTERM.
$ agent-inbox serve --reset-interval 10s

# In another terminal, inspect durable scheduler state.
$ agent-inbox status
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
internal/server/           Reset scheduling, recovery, and automatic continuation
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
attempt usage, and halt state exactly as persisted. Agent-error resumes start a
fresh configured window. Token-exhausted resumes are rejected until their durable
absolute eligibility timestamp. Interrupted attempts resume immediately with only
the allowance remaining in their current window. A resumed attempt may still end
in `failed`; that outcome is recorded and printed explicitly. As with `run`, a
completed attempt that records task failure does not make the command itself fail.
Rejected or conflicting invocations do return a non-zero exit status.

### Server and reset model

`serve` is a foreground scheduler behind a small interface: construct it with
configuration and dependencies, then call `Run(ctx)`. It periodically rescans at a
capped interval so it notices work produced by other CLI processes without a hot
loop. SQLite remains the sole coordinator.

An executor that observes genuine token exhaustion writes `next_eligible_at` on
the task. That absolute timestamp is authoritative even if another process uses a
different reset interval. `serve` initializes older unscheduled exhausted tasks
from the terminal run's `finished_at` and its configured interval. The global flag
or `AGENT_INBOX_RESET_INTERVAL` gives `run`, `resume`, and `work` the same setting;
the command-specific `serve --reset-interval` controls timestamps created by the
server and legacy-state initialization.

Only genuine token exhaustion is retried automatically. Agent errors are terminal
for automatic policy. Progress means a positive completed-step delta during the
attempt. Because the plan is finite and every scheduled retry must advance at
least one step, retries are naturally bounded by the remaining plan. A completely
fresh window that advances zero steps is durably marked `stopped` with:

```
auto-retry stopped: next step requires more than the configured window
```

Graceful interruption and recovered crashes are immediately continuable and keep
the current window's remaining allowance. Persistence or session I/O failures are
returned as operational errors; they are not mislabeled as agent errors.

### Ownership, recovery, and reconciliation

Every running run owns a cryptographically random token and a renewable three-
second lease; executors renew once per second. Renewal, progress publication, and
finalization are all guarded by run ID, owner token, and lease validity. If an
owner loses its lease, its context is cancelled and stale writes are rejected.
Task claims and run creation remain one SQLite compare-and-swap transaction, which
also resolves races between `serve` and human commands.

This follows the owner-token and fencing rationale described by the
[Redis locking documentation](https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/)
and [Hazelcast FencedLock documentation](https://docs.hazelcast.com/hazelcast/5.0/data-structures/fencedlock).
SQLite WAL permits concurrent readers but still serializes writers; see SQLite's
[transaction](https://www.sqlite.org/lang_transaction.html) and
[WAL](https://sqlite.org/wal.html) documentation.

The session file records the current run identity, run-start step, completed step
outputs, remaining allowance, and explicit halt reason. After a lease expires,
`serve` reconciles that checkpoint before creating a replacement run:

- completed checkpoints become succeeded runs;
- token exhaustion becomes scheduled or stopped according to completed-step delta;
- agent errors become terminal errored runs;
- interrupted or mid-step checkpoints become interrupted runs eligible immediately.

Therefore every session step durably persisted before a process crash is
attributed to exactly one historical run. This covers normal process termination
and SIGKILL. It does not promise recovery from disk corruption, filesystem or
hardware durability failures, or exactly-once side effects in a real external
agent. A crash after the database run is inserted but before session binding is
also recovered as an empty interrupted attempt with its intended allowance.

### Observability

Server logs are structured JSON and report startup, the next wake, claims,
recovery, attempt outcomes, new eligibility, and stopped reasons. `status` reports
durable state: the current task/run and lease expiry, the next eligible task and
timestamp, and stopped tasks. It explicitly does not prove that a server process
is alive; that would require a separate heartbeat guarantee.

## Known limitations

- Crash takeover waits for the old three-second lease to expire; status may show
  the abandoned run during that interval.
- Status describes durable scheduler state, not server liveness. There is no
  heartbeat or remote control endpoint.
- Reset timing uses wall-clock timestamps. Large system-clock jumps can change
  when work becomes eligible.
- Session JSON and SQLite are separate durability domains. Reconciliation closes
  the process-crash gap, but disk corruption and power-loss durability depend on
  the host filesystem and SQLite configuration.
- The simulated agent is deterministic — the same task title and description
  always produce the same plan and step costs.
