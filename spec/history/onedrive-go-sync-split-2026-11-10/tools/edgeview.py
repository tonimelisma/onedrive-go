#!/usr/bin/env python3
"""Compact per-edge view of edges.tsv: members collapsed onto their owning declaration.

usage: edgeview.py <out-dir> [from_pkg] [to_pkg]
"""
import csv
import sys
from collections import defaultdict

out = sys.argv[1]
want_from = sys.argv[2] if len(sys.argv) > 2 else None
want_to = sys.argv[3] if len(sys.argv) > 3 else None

edges = defaultdict(lambda: defaultdict(lambda: {"kind": "", "exp": True, "file": "", "sites": 0, "members": [], "unexp_members": []}))
with open(f"{out}/edges.tsv") as f:
    for r in csv.DictReader(f, delimiter="\t"):
        key = (r["from_pkg"], r["to_pkg"])
        sym = r["symbol"]
        owner, _, member = sym.partition(".")
        d = edges[key][owner]
        d["sites"] += int(r["sites"])
        if member:
            d["members"].append(member)
            if r["exported"] != "true":
                d["unexp_members"].append(f"{member}({r['kind']})")
            if not d["file"]:
                d["file"] = r["sym_file"]
            if not d["kind"]:
                d["kind"] = "type"
        else:
            d["kind"] = r["kind"]
            d["exp"] = r["exported"] == "true"
            d["file"] = r["sym_file"]

for key in sorted(edges):
    if want_from and key[0] != want_from:
        continue
    if want_to and key[1] != want_to:
        continue
    owners = edges[key]
    print(f"\n=== {key[0]} -> {key[1]}: {len(owners)} declarations, {sum(d['sites'] for d in owners.values())} sites")
    for owner in sorted(owners, key=lambda o: (owners[o]["file"], o)):
        d = owners[owner]
        first = owner[0]
        owner_exported = first.isupper()
        mark = " " if owner_exported else "*"
        extra = ""
        if d["members"]:
            extra = f" members={len(d['members'])}"
        if d["unexp_members"]:
            extra += " unexported:" + ",".join(d["unexp_members"])
        print(f"  {mark} {d['file']:<40} {d['kind']:<6} {owner}  sites={d['sites']}{extra}")
