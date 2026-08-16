#!/usr/bin/env bash
set -euo pipefail

BIN="./bin/agent-inbox"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

export AGENT_INBOX_DATA_DIR="$TMPDIR"

echo "=== smoke test ==="

# Add tasks
ID1=$($BIN add "simple task" -d "[steps:3] [budget:5000]")
echo "added task $ID1"

ID2=$($BIN add "failing task" -d "[steps:5] [fail-at:2] [budget:5000]")
echo "added task $ID2"

ID3=$($BIN add "big task" -d "[steps:20] [budget:500]")
echo "added task $ID3"

# Status before work
echo ""
echo "--- status before work ---"
$BIN status

# List
echo ""
echo "--- list ---"
$BIN list

# Work through all tasks
echo ""
echo "--- work ---"
$BIN work 2>&1 || true

# Status after work
echo ""
echo "--- status after work ---"
$BIN status

# Verify statuses
echo ""
echo "--- verifying statuses ---"

STATUS1=$($BIN list --status done | grep -c "simple task" || true)
if [ "$STATUS1" -eq 0 ]; then
    echo "FAIL: simple task should be done"
    exit 1
fi
echo "OK: simple task is done"

STATUS2=$($BIN list --status failed | grep -c "failing task" || true)
if [ "$STATUS2" -eq 0 ]; then
    echo "FAIL: failing task should be failed"
    exit 1
fi
echo "OK: failing task is failed"

STATUS3=$($BIN list --status failed | grep -c "big task" || true)
if [ "$STATUS3" -eq 0 ]; then
    echo "FAIL: big task should be failed (token exhaustion)"
    exit 1
fi
echo "OK: big task is failed (token exhaustion)"

# Show details of the simple task
echo ""
echo "--- show task $ID1 ---"
$BIN show "$ID1"

# Continue the done task
echo ""
echo "--- continue task $ID1 ---"
$BIN continue "$ID1" -m "add better error handling"

# Show after continue
echo ""
echo "--- show task $ID1 after continue ---"
$BIN show "$ID1"

# Verify it ran again
RUNS=$($BIN show "$ID1" | grep -c "Run #" || true)
if [ "$RUNS" -lt 2 ]; then
    echo "FAIL: expected at least 2 runs after continue"
    exit 1
fi
echo "OK: task has $RUNS runs after continue"

echo ""
echo "=== all smoke tests passed ==="
