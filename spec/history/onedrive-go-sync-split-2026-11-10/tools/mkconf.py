#!/usr/bin/env python3
"""mkconf.py <prod-conf> <tests-conf> <out-dir-of-prod-run> <result-conf>

Joins a production configuration and a solved test configuration into one configuration
for endstate.go (whose `helper` directive takes names only; imports go through order/allow).
"""
import os
import subprocess
import sys

S = os.path.dirname(os.path.abspath(__file__)) + "/"
prod, tests, out, result = sys.argv[1:5]
lines = []
order = None
for line in open(prod):
    if line.startswith("order "):
        order = line.split()[1:]
        continue
    lines.append(line.rstrip("\n"))
helpers = []
tail = []
for line in open(tests):
    body = line.split("#", 1)[0].split()
    if not body:
        continue
    k = body[0]
    if k == "helper":
        helpers.append(body[1])
        tail.append("allow " + " ".join(body[1:]))
    elif k in ("testfile", "testexport"):
        tail.append(" ".join(body))
    # testdecl/testcopy come from the solver's emit; auto/solve are evaluator-only
emit = subprocess.run([sys.executable, S + "evaltests.py", out, tests, "emit"], check=True, capture_output=True, text=True).stdout
with open(result, "w") as f:
    f.write("order " + " ".join(order + helpers) + "\n")
    f.write("\n".join(lines) + "\n\n# ---- tests ----\n")
    f.write("helper " + " ".join(helpers) + "\n")
    f.write("\n".join(tail) + "\n")
    f.write(emit)
