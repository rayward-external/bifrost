#!/usr/bin/env bash
# Fork-only helper (not in upstream): turn a saved `go test -v` log into a FAIL
# SUMMARY printed as the LAST lines of the step, plus at most 10 ::error
# annotations.
#
# Two readers depend on it:
#   - A reader that only sees the tail of a job log. `go test` prints a
#     package's output when that package finishes, so a failing test in an early
#     package lands thousands of lines above the tail, under the -v and coverage
#     output of every package after it.
#   - The check-run annotations API. GitHub keeps at most 10 error annotations
#     per step, so they are spent on test names first.
#
# Usage: go-test-fail-summary.sh <go-test-log> <go-test-exit-status>
#
# Always exits 0 and never decides the step's result: the caller keeps go
# test's own exit status and returns it after this prints.
set -uo pipefail

usage="usage: go-test-fail-summary.sh <go-test-log> <go-test-exit-status>"
log="${1:?$usage}"
status="${2:?$usage}"
max_annotations=10
max_summary_lines=150

if [ "$status" = "0" ]; then
  echo "FAIL SUMMARY: go test exited 0; no failing test."
  exit 0
fi

if [ ! -r "$log" ]; then
  echo "::error title=go test::go test exited $status and its saved output ($log) is unreadable"
  echo "FAIL SUMMARY: go test exited $status; its saved output ($log) is unreadable."
  exit 0
fi

# Annotations, in priority order: failing top-level tests (the names a reader
# needs), then panics (a test timeout prints no --- FAIL line), then failing
# packages (which also covers "[build failed]"), then one line counting data
# races. Duplicates are dropped and the list is cut at the annotation cap.
awk -v max="$max_annotations" '
  function esc(s) {
    gsub(/%/, "%25", s); gsub(/\r/, "%0D", s); gsub(/\t/, " ", s)
    return s
  }
  /^--- FAIL:/              { if (!seen[$0]++) fail[++nf] = $0; next }
  /^panic:/                 { if (!seen[$0]++) pan[++np] = $0; next }
  /^FAIL\t/                 { if (!seen[$0]++) pkg[++nk] = $0; next }
  /WARNING: DATA RACE/      { races++ }
  END {
    n = 0
    for (i = 1; i <= nf && n < max; i++) { print "::error title=go test FAIL::" esc(fail[i]); n++ }
    for (i = 1; i <= np && n < max; i++) { print "::error title=go test panic::" esc(pan[i]); n++ }
    for (i = 1; i <= nk && n < max; i++) { print "::error title=go test FAIL::" esc(pkg[i]); n++ }
    if (races && n < max) { print "::error title=go test DATA RACE::WARNING: DATA RACE reported " races " time(s); the test it ran in has a --- FAIL line"; n++ }
    if (n == 0) { print "::error title=go test::go test exited non-zero with no --- FAIL, panic, FAIL package or DATA RACE line; see the summary at the end of this step" }
  }
' "$log"

# The summary keeps log order, so subtests stay under their parent. A test
# timeout prints only its panic and the tests still running, so those are kept.
summary="$(awk '
  /^panic:/                 { print; timeout = ($0 ~ /test timed out/); next }
  timeout && /^[ \t]/       {
    line = $0; sub(/^[ \t]+/, "", line)
    if (line != "" && line != "running tests:") print "    still running: " line
    next
  }
                            { timeout = 0 }
  /^[ \t]*--- FAIL:/        { print; next }
  /^FAIL\t/                 { print; next }
  /WARNING: DATA RACE/      { races++ }
  END {
    if (races) print "WARNING: DATA RACE reported " races " time(s); each one fails the test it ran in (its --- FAIL line is above)"
  }
' "$log")"

echo
echo "==================== FAIL SUMMARY (go test exit status $status) ===================="
if [ -n "$summary" ]; then
  total="$(printf '%s\n' "$summary" | wc -l | tr -d ' ')"
  printf '%s\n' "$summary" | head -n "$max_summary_lines"
  if [ "$total" -gt "$max_summary_lines" ]; then
    echo "... and $((total - max_summary_lines)) more matching line(s); search this log for --- FAIL"
  fi
else
  echo "No --- FAIL, panic, FAIL package or DATA RACE line matched. Last 20 lines of go test output:"
  tail -n 20 "$log"
fi
echo "===================================================================================="
exit 0
