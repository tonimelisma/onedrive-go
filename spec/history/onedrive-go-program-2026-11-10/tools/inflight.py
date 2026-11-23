#!/usr/bin/env python3
"""Print the plan rows that are in flight: status neither "planned" nor "merged ...".

usage: inflight.py [WIDTH]     (WIDTH cuts each status, default 260)
       inflight.py --count     (prints only merged / in flight / planned counts)
"""
import os
import re
import sys

PLAN = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "PLAN.md")
ROW = re.compile(r"^\| ([A-Z]\d+) \|")


def rows():
    for line in open(PLAN, encoding="utf-8"):
        if not ROW.match(line):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 5:
            continue
        yield cells[0], cells[4]


def main():
    width = 260
    count_only = False
    for arg in sys.argv[1:]:
        if arg == "--count":
            count_only = True
        else:
            width = int(arg)
    merged = planned = 0
    flying = []
    for ident, status in rows():
        low = status.lower()
        if low.startswith("merged") or low.startswith("done"):
            merged += 1
        elif low in ("planned", ""):
            planned += 1
        else:
            flying.append((ident, status))
    print(f"merged {merged}, in flight {len(flying)}, planned {planned}")
    if count_only:
        return
    for ident, status in flying:
        cut = status if len(status) <= width else status[:width] + "…"
        print(f"{ident}: {cut}")


if __name__ == "__main__":
    main()
