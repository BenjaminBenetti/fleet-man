# BSD userland shims

PATH-prepended fakes that make GNU/Linux hosts behave like macOS/BSD for the
commands fleet (and the test harness) shell out to on the HOST. Each shim
rejects GNU-only usage the way BSD does, then delegates to the next real
binary on PATH. Commands run INSIDE containers are untouched -- containers
are Linux even on a Mac, so their GNU userland is correct.

Activated by FLEET_ITEST_BSD_SHIMS=1 (see integration/run.sh). Purpose:
catch the #231 bug class -- GNU-isms in host-side shell-outs (e.g.
`sleep infinity`) -- across the FULL suite on cheap Linux runners, no Mac
hardware required. The idea comes from the QA bot's validation of PR #235.

- sleep: rejects non-numeric operands (`sleep infinity` is GNU-only)
- stat: rejects -c/--format/--printf, and emulates BSD -f for the formats
  the suite uses (%Lp, %z) when the real stat is GNU -- so portable
  try-GNU-fall-back-to-BSD probes behave as on a real Mac
- date: rejects -d/--date (BSD spells it -v/-r)
- timeout: always "command not found" (macOS ships no timeout(1))
