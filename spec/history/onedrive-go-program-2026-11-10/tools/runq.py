#!/usr/bin/env python3
"""Run a command, keep its whole output in a log file, print only the end.

Long build and test output stays out of the conversation: the log has all of
it, and only the exit status, the run time and the last lines are printed.

usage: runq.py [-n LINES] [-C DIR] [-e KEY=VALUE]... [-g REGEX] LOG -- CMD [ARG...]

  -n LINES   lines of the end of the output to print (default 25)
  -C DIR     run the command in DIR
  -e K=V     add K=V to the environment (PATH=... replaces PATH)
  -g REGEX   also print up to 40 output lines that match REGEX
"""
import os
import re
import subprocess
import sys
import time

WIDTH = 300
MATCH_LIMIT = 40


def main() -> int:
    args = sys.argv[1:]
    tail, cwd, pattern = 25, None, None
    env = dict(os.environ)
    while args and args[0] in ("-n", "-C", "-e", "-g"):
        flag, value = args[0], args[1]
        args = args[2:]
        if flag == "-n":
            tail = int(value)
        elif flag == "-C":
            cwd = os.path.expanduser(value)
        elif flag == "-g":
            pattern = re.compile(value)
        else:
            key, _, val = value.partition("=")
            env[key] = os.path.expandvars(val)
    if len(args) < 3 or args[1] != "--":
        print(__doc__)
        return 2
    log, cmd = args[0], args[2:]

    start = time.monotonic()
    with open(log, "w", encoding="utf-8") as out:
        proc = subprocess.run(
            cmd, cwd=cwd, env=env, stdout=out, stderr=subprocess.STDOUT, check=False
        )
    elapsed = time.monotonic() - start

    with open(log, encoding="utf-8", errors="replace") as fh:
        lines = fh.read().splitlines()
    print(f"exit {proc.returncode} after {elapsed:.0f}s, {len(lines)} lines in {log}")
    if pattern is not None:
        hits = [ln for ln in lines if pattern.search(ln)]
        for ln in hits[:MATCH_LIMIT]:
            print("| " + ln[:WIDTH])
        if len(hits) > MATCH_LIMIT:
            print(f"| ... {len(hits) - MATCH_LIMIT} more matching lines")
    for ln in lines[-tail:]:
        print(ln[:WIDTH])
    return proc.returncode


if __name__ == "__main__":
    sys.exit(main())
