#!/usr/bin/env python3
"""ad-hoc queries over the solved model.

q.py <prod-out-dir> <tests-conf> decl <id...>            end-state package of declarations
q.py <prod-out-dir> <tests-conf> why <entry> <pkg...>    which production symbols of <pkg> an entry point reaches, and through what
q.py <prod-out-dir> <tests-conf> users <root>            entry points (with home) that reach a test root
q.py <prod-out-dir> <tests-conf> refs <root>             production references of one test root (direct)
q.py <prod-out-dir> <tests-conf> who <symbol>            test declarations referencing a production symbol

<prod-out-dir> is the simulator's output for conf/production.txt (it must hold rawdecls.tsv and rawrefs.tsv).
"""
import collections
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import evaltests as E  # noqa: E402

out, confpath = sys.argv[1], sys.argv[2]
args = sys.argv[3:]
conf = E.Conf(confpath)
m = E.Model(out, conf)
mode = args[0]
if mode == "decl":
    for i in args[1:]:
        d = m.decls.get(i)
        print(i, "->", (m.pkg[i], d["kind"], d["file"], d["loc"], "test" if d["test"] else "prod", m.how.get(i, d["how"])) if d else None)
elif mode == "why":
    entry, pk = args[1], set(args[2:])
    prev = {entry: None}
    dq = collections.deque([entry])
    while dq:
        x = dq.popleft()
        for y in m.tedges.get(x, ()):
            if y not in prev:
                prev[y] = x
                dq.append(y)
    seen = set()
    for x in prev:
        for q, sym, exp, sites, to in m.prodrefs.get(x, ()):
            if (not pk or q in pk) and sym not in seen:
                seen.add(sym)
                path, c = [], x
                while c is not None:
                    path.append(c)
                    c = prev[c]
                print(f"{q}\t{sym}\t{'exported' if exp or sym in conf.testexport else 'UNEXPORTED'}\t{' <- '.join(path)}")
elif mode == "users":
    for root in args[1:]:
        c = collections.Counter()
        names = collections.defaultdict(list)
        for i, d in m.decls.items():
            if E.is_entry(d) and root in m.closure(i):
                c[m.pkg[i]] += 1
                names[m.pkg[i]].append(f"{i} ({d['file']})")
        print(root, dict(c))
        for p, v in names.items():
            if len(v) <= 12 or "-v" in args:
                for n in v:
                    print("   ", p, n)
elif mode == "refs":
    for root in args[1:]:
        for q, sym, exp, sites, to in sorted(m.prodrefs.get(root, ())):
            print(f"{root}\t{q}\t{sym}\t{'exported' if exp or sym in conf.testexport else 'UNEXPORTED'}\t{sites}")
        print(root, "test refs:", sorted(m.tedges.get(root, ())))
elif mode == "who":
    for sym in args[1:]:
        for r in m.refs:
            if r["symbol"] == sym:
                fd = m.decls[r["from_decl"]]
                print(f"{sym}\t{r['from_decl']}\t{fd['file']}\thome={m.pkg[fd['id']]}\tsites={r['sites']}")
