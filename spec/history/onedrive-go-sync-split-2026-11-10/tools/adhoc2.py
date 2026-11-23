#!/usr/bin/env python3
"""adhoc2.py <out-dir> <mode> [args]: further queries over the simulator's ledgers."""
import collections
import csv
import sys

out, mode = sys.argv[1], sys.argv[2]
args = sys.argv[3:]


def rows(name):
    with open(f"{out}/{name}") as f:
        return list(csv.DictReader(f, delimiter="\t", quoting=csv.QUOTE_NONE))


if mode == "testprefix":
    # where do test declarations with this name prefix end up?
    c = collections.Counter()
    for r in rows("testdecls.tsv"):
        if r["decl"].startswith(args[0]):
            c[r["pkg"]] += 1
            if r["pkg"] != "sync":
                print(r["decl"], r["pkg"], r["file"])
    print(dict(c))
elif mode == "edge":
    # every symbol crossing one production edge
    a, b = args
    for r in rows("edges.tsv"):
        if r["from_pkg"] == a and r["to_pkg"] == b:
            print(f"{r['symbol']}\t{r['kind']}\t{'exported' if r['exported'] == 'true' else 'unexported'}\t{r['sym_file']}\t{r['sites']}")
elif mode == "prodfiles":
    # production files and the packages their declarations end up in
    by = collections.defaultdict(collections.Counter)
    for r in rows("rawdecls.tsv"):
        if r["test"] == "false":
            by[r["file"]][r["pkg"]] += 1
    print(len(by), "production files with declarations")
elif mode == "exportsof":
    # identifiers to export in one package, grouped by kind
    by = collections.defaultdict(list)
    for r in rows("exports.tsv"):
        if r["pkg"] == args[0]:
            by[r["kind"]].append(r["symbol"])
    for k, v in sorted(by.items()):
        print(f"{k} ({len(v)}): " + " ".join(sorted(v)))
elif mode == "testmatch":
    # test functions matching a regular expression, counted by end-state package
    import re
    pat = re.compile(args[0])
    c = collections.Counter()
    for r in rows("testdecls.tsv"):
        if r["kind"] == "func" and r["decl"].startswith("Test") and pat.search(r["decl"]):
            c[r["pkg"]] += 1
    print(dict(c))
elif mode == "testcount":
    c = collections.Counter()
    for r in rows("testdecls.tsv"):
        if r["kind"] == "func" and (r["decl"].startswith("Test") or r["decl"].startswith("Benchmark") or r["decl"].startswith("Fuzz")):
            c[(r["pkg"], r["decl"][:4])] += 1
    for k, v in sorted(c.items()):
        print(k, v)
