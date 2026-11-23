#!/usr/bin/env python3
"""adhoc.py <out-dir> <mode> [args]: small queries over the simulator's ledgers."""
import collections
import csv
import sys

out, mode = sys.argv[1], sys.argv[2]
args = sys.argv[3:]


def rows(name):
    with open(f"{out}/{name}") as f:
        return list(csv.DictReader(f, delimiter="\t", quoting=csv.QUOTE_NONE))


if mode == "testedge":
    a, b = args
    for r in rows("testedges.tsv"):
        if r["from_pkg"] == a and r["to_pkg"] == b:
            print(f"{r['symbol']}\t{r['kind']}\t{'exported' if r['exported_today'] == 'true' else 'unexported'}\t{r['sym_file']}\t{r['sites']}")
elif mode == "files":
    by = collections.defaultdict(lambda: collections.Counter())
    for r in rows("rawdecls.tsv"):
        if r["test"] == "false":
            by[r["file"]][r["pkg"]] += int(r["loc"])
    per = collections.defaultdict(list)
    for f, c in sorted(by.items()):
        if len(c) == 1:
            per[next(iter(c))].append(f)
    for p, fs in per.items():
        print(f"{p} ({len(fs)} whole files)")
        print("   " + " ".join(fs))
elif mode == "edgekinds":
    c = collections.defaultdict(lambda: collections.Counter())
    for r in rows("edges.tsv"):
        c[(r["from_pkg"], r["to_pkg"])][(r["kind"], r["exported"])] += 1
    for k, v in c.items():
        exp = sum(n for (kind, e), n in v.items() if e == "true")
        new = collections.Counter()
        for (kind, e), n in v.items():
            if e != "true":
                new[kind] += n
        print(f"{k[0]} -> {k[1]}: {exp} already exported, to export {dict(sorted(new.items()))}")
elif mode == "helpercollide":
    seen = collections.defaultdict(list)
    for r in rows("helpers.tsv"):
        n = r["helper_decl"]
        if "." in n:
            continue
        seen[(r["helper_pkg"], n[:1].upper() + n[1:])].append(n)
    for k, v in seen.items():
        if len(v) > 1:
            print("collision", k, v)
    print("checked", len(seen))
elif mode == "testfilesplit":
    for r in rows("tests.tsv"):
        if r["loc_by_pkg"].count("=") > 1:
            print(f"{r['test_file']}\t{r['loc_by_pkg']}")
elif mode == "testhome":
    c = collections.Counter()
    for r in rows("tests.tsv"):
        parts = r["loc_by_pkg"].split()
        c[(len(parts) > 1)] += 1
    print(c)
