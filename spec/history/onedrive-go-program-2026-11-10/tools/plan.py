#!/usr/bin/env python3
"""Read and update PLAN.md without loading the whole file into a conversation.

usage:
  plan.py status            one short line per PR: id, lane, state, PR, commit
  plan.py open              only the PRs that are not merged or dropped
  plan.py show ID           the status row and the plan entry for one PR
  plan.py set ID STATE [PR] [COMMIT]
                            rewrite the state (and PR number, merge commit) of
                            one status row; STATE may hold spaces if quoted
"""
import os
import re
import sys

PLAN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "PLAN.md")
ROW = re.compile(r"^\| ([A-Z]\d+) \|")
CLOSED = ("merged", "dropped", "not needed")


def read_lines():
    with open(PLAN, encoding="utf-8") as fh:
        return fh.read().split("\n")


def status_rows(lines):
    """Rows of the section 1 table: (line index, cells)."""
    rows = []
    in_status = False
    for idx, line in enumerate(lines):
        if line.startswith("## "):
            in_status = line.startswith("## 1.")
            continue
        if in_status and ROW.match(line):
            cells = [c.strip() for c in line.strip().strip("|").split("|")]
            rows.append((idx, cells))
    return rows


def cmd_status(only_open):
    for _, cells in status_rows(read_lines()):
        ident, title, _branch, lane, state, pr, commit = cells[:7]
        if only_open and state.startswith(CLOSED):
            continue
        tail = " ".join(x for x in (("#" + pr) if pr else "", commit) if x)
        print(f"{ident:<4}{lane} {state:<28} {tail:<18} {title[:70]}")


def cmd_show(ident):
    lines = read_lines()
    for _, cells in status_rows(lines):
        if cells[0] == ident:
            print("STATUS: " + " | ".join(cells))
    heading = re.compile(r"^#### " + re.escape(ident) + r"\.")
    start = next((i for i, ln in enumerate(lines) if heading.match(ln)), None)
    if start is not None:
        end = start + 1
        while end < len(lines) and not lines[end].startswith(("#### ", "### ", "## ")):
            end += 1
        print("\n".join(lines[start:end]).rstrip())
        return
    # Waves written as tables: print the row, with the wave's lead-in.
    wave = None
    for idx, line in enumerate(lines):
        if line.startswith("### Wave"):
            wave = idx
        if wave is not None and line.startswith(f"| {ident} |") and idx > wave:
            if any(i == idx for i, _ in status_rows(lines)):
                continue
            lead = wave
            while lead < len(lines) and not lines[lead].startswith("|"):
                lead += 1
            print("\n".join(lines[wave:lead]).rstrip())
            print(lines[lead])
            print(line)
            return
    print(f"no plan entry for {ident}")


def cmd_set(ident, state, pr, commit):
    lines = read_lines()
    for idx, cells in status_rows(lines):
        if cells[0] != ident:
            continue
        cells = (cells + ["", "", ""])[:7]
        cells[4] = state
        if pr is not None:
            cells[5] = pr.lstrip("#")
        if commit is not None:
            cells[6] = f"`{commit.strip('`')}`"
        lines[idx] = "| " + " | ".join(cells) + " |"
        with open(PLAN, "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines))
        print(lines[idx])
        return 0
    print(f"no status row for {ident}")
    return 1


def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__)
        return 2
    if args[0] == "status":
        cmd_status(False)
    elif args[0] == "open":
        cmd_status(True)
    elif args[0] == "show" and len(args) == 2:
        cmd_show(args[1])
    elif args[0] == "set" and len(args) >= 3:
        return cmd_set(
            args[1],
            args[2],
            args[3] if len(args) > 3 else None,
            args[4] if len(args) > 4 else None,
        )
    else:
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
