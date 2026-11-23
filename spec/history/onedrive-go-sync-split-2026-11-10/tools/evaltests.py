#!/usr/bin/env python3
"""Offline evaluator for the test configuration of the end state.

Reads rawdecls.tsv / rawrefs.tsv written by endstate.go (production assignment fixed) and a
test configuration, then re-implements the simulator's test-declaration assignment and
checkTestRef so a configuration can be iterated in seconds.

usage: evaltests.py <out-dir> <tests-conf> <mode> [args]
  summary                 violations by class and package pair
  syms [class] [p] [q]    violations grouped by symbol
  files                   violations per test file
  file <file>             every violation in one test file
  suggest                 for every entry point: a legal home with the fewest seams
  shared                  shared helper roots: users, required imports
  emit                    directives for endstate.go
"""
import collections
import csv
import sys

PROD_ORDER = ["syncdomain", "syncshortcut", "syncstore", "syncplan", "syncobserve", "syncexec", "sync"]
PROD_ALLOW = {
    "syncdomain": set(),
    "syncshortcut": {"syncdomain"},
    "syncstore": {"syncdomain", "syncshortcut"},
    "syncplan": {"syncdomain"},
    "syncobserve": {"syncdomain", "syncshortcut"},
    "syncexec": {"syncdomain"},
    "sync": {"syncdomain", "syncshortcut", "syncstore", "syncplan", "syncobserve", "syncexec"},
}
ENTRY_PREFIXES = ("Test", "Benchmark", "Fuzz", "Example")


class Conf:
    def __init__(self, path):
        self.order = list(PROD_ORDER)
        self.allow = {p: set(v) for p, v in PROD_ALLOW.items()}
        self.helper = []
        self.testfile = {}
        self.testdecl = {}
        self.testcopy = collections.defaultdict(set)
        self.testexport = set()
        self.auto = False
        self.solve = False
        if not path or path == "-":
            return
        for line in open(path):
            line = line.split("#", 1)[0].strip()
            if not line:
                continue
            f = line.split()
            k = f[0]
            if k == "helper":  # helper <pkg> <imports...>
                self.helper.append(f[1])
                self.order.append(f[1])
                self.allow[f[1]] = set(f[2:])
            elif k == "testfile":
                for x in f[2:]:
                    self.testfile[x] = f[1]
            elif k == "testdecl":
                for x in f[2:]:
                    self.testdecl[x] = f[1]
            elif k == "testcopy":
                for x in f[2:]:
                    self.testcopy[f[1]].add(x)
            elif k == "testexport":
                self.testexport |= set(f[1:])
            elif k == "auto":
                self.auto = True
            elif k == "solve":
                self.solve = True
                self.auto = True
            else:
                raise SystemExit(f"unknown directive {k}")
        self.reach = {}
        for p in self.order:
            seen = set()
            stack = list(self.allow[p])
            while stack:
                q = stack.pop()
                if q in seen:
                    continue
                seen.add(q)
                stack.extend(self.allow[q])
            self.reach[p] = seen


def load(out):
    decls = {}
    with open(out + "/rawdecls.tsv") as f:
        for r in csv.DictReader(f, delimiter="\t"):
            r["test"] = r["test"] == "true"
            r["loc"] = int(r["loc"])
            decls[r["id"]] = r
    refs = []
    with open(out + "/rawrefs.tsv") as f:
        for r in csv.DictReader(f, delimiter="\t"):
            r["sites"] = int(r["sites"])
            r["exp"] = r["exported_end"] == "true"
            refs.append(r)
    return decls, refs


def is_entry(d):
    return d["test"] and d["kind"] == "func" and d["id"].startswith(ENTRY_PREFIXES)


def copy_root(decls, d):
    if d["kind"] == "method":
        return d["recv"]
    if d["kind"] == "const" and d["typeof"]:
        t = decls.get(d["typeof"])
        if t is not None and t["test"]:
            return d["typeof"]
    return d["id"]


class Model:
    def __init__(self, out, conf):
        self.decls, self.refs = load(out)
        self.conf = conf
        decls = self.decls
        # file default home: the package its default-assigned declarations got from the family rule
        self.filedef = {}
        for d in decls.values():
            if d["test"] and d["how"] == "default":
                self.filedef[d["file"]] = d["pkg"]
        self.root = {i: copy_root(decls, d) for i, d in decls.items() if d["test"]}
        # test-to-test edges between roots, and production needs per root
        self.tedges = collections.defaultdict(set)
        self.prodrefs = collections.defaultdict(list)  # root -> [(q, sym, exp, sites, to)]
        self.byfrom = collections.defaultdict(list)
        for r in self.refs:
            fr = self.root[r["from_decl"]]
            td = decls[r["to_decl"]]
            self.byfrom[r["from_decl"]].append(r)
            if td["test"]:
                tr = self.root[td["id"]]
                if tr != fr:
                    self.tedges[fr].add(tr)
            else:
                self.prodrefs[fr].append((td["pkg"], r["symbol"], r["exp"], r["sites"], td["id"]))
        self.entry_log = {}
        if conf.solve:
            self.solve_entries()
        self.assign()

    def entry_needs(self, i):
        needs = collections.defaultdict(set)
        pk = set()
        for x in self.closure(i):
            for q, sym, exp, sites, to in self.prodrefs.get(x, ()):
                pk.add(q)
                if not (exp or sym in self.conf.testexport):
                    needs[q].add(sym)
        return pk, needs

    def solve_entries(self):
        """home of every entry point: a testfile directive is forced when legal; otherwise the legal
        home with the fewest unexported symbols of other packages (ties: the file's home, then lowest)"""
        conf = self.conf
        for i, d in self.decls.items():
            if not is_entry(d) or i in conf.testdecl:
                continue
            pk, needs = self.entry_needs(i)
            legal = [h for h in PROD_ORDER if all(q == h or h not in conf.reach[q] for q in pk)]
            pref = self.file_home(d["file"])

            def cost(h):
                return sum(len(v) for q, v in needs.items() if q != h)

            if d["file"] in conf.testfile and pref in legal:
                best = pref
            else:
                best = min(legal, key=lambda h: (cost(h), h != pref, PROD_ORDER.index(h)))
            self.entry_log[i] = (pref, best, cost(pref) if pref in legal else -1, cost(best))
            if best != pref:
                conf.testdecl[i] = best

    def exported_end(self, r):
        return r["exp"] or r["symbol"] in self.conf.testexport

    def file_home(self, f):
        return self.conf.testfile.get(f, self.filedef[f])

    def closure(self, root, memo={}):
        if root in memo:
            return memo[root]
        seen, stack = set(), [root]
        while stack:
            x = stack.pop()
            if x in seen:
                continue
            seen.add(x)
            stack.extend(self.tedges.get(x, ()))
        memo[root] = seen
        return seen

    def assign(self):
        decls, conf = self.decls, self.conf
        pkg = {}
        how = {}
        for i, d in decls.items():
            if not d["test"]:
                pkg[i] = d["pkg"]
                continue
            pkg[i], how[i] = self.file_home(d["file"]), "default"
            if i in conf.testdecl:
                pkg[i], how[i] = conf.testdecl[i], "testdecl"
        if conf.auto:
            self.auto_place(pkg, how)
        for i, d in decls.items():
            if d["test"] and d["kind"] == "const" and d["typeof"] and how.get(i) != "testdecl":
                t = decls.get(d["typeof"])
                if t is not None and t["test"] and pkg[t["id"]] != pkg[i]:
                    pkg[i], how[i] = pkg[t["id"]], "const-follows-type"
        for i, d in decls.items():
            if d["test"] and d["kind"] == "method":
                t = decls.get(d["recv"])
                if t is not None and pkg[t["id"]] != pkg[i]:
                    pkg[i], how[i] = pkg[t["id"]], "method-follows-type"
        self.pkg, self.how = pkg, how

    # ---- automatic placement of helper roots (entry points keep their configured home) ----
    def entry_homes(self, pkg):
        return {i: pkg[i] for i, d in self.decls.items() if is_entry(d)}

    def users_of_roots(self, pkg):
        users = collections.defaultdict(collections.Counter)
        for e, h in self.entry_homes(pkg).items():
            for x in self.closure(e):
                if x != e:
                    users[x][h] += 1
        return users

    def need(self, root):
        """production packages the root needs, through its closure of test helpers"""
        out = set()
        for x in self.closure(root):
            for q, _, _, _, _ in self.prodrefs.get(x, ()):
                out.add(q)
        return out

    def helper_for(self, need):
        for h in self.conf.helper:
            if need <= (self.conf.reach[h] | {h}):
                return h
        return None

    def auto_place(self, pkg, how):
        conf, decls = self.conf, self.decls
        users = self.users_of_roots(pkg)
        self.auto_copies = collections.defaultdict(set)
        self.unplaced = {}
        for root, us in users.items():
            d = decls[root]
            if is_entry(d) or root in conf.testdecl:
                continue
            if len(us) == 1:
                (p,) = us
                if pkg[root] != p:
                    pkg[root], how[root] = p, "testdecl"
                continue
            h = self.helper_for(self.need(root))
            if h is None:
                self.unplaced[root] = us
                continue
            pkg[root], how[root] = h, "testdecl"
            for p in us:
                if p in conf.reach[h]:
                    self.auto_copies[p].add(root)
        # roots no entry point reaches stay with their file
        for p, s in self.auto_copies.items():
            conf.testcopy[p] |= s
        # non-root members (untyped consts etc. are their own roots; methods/typed consts follow)

    # ---- checking ----
    def check(self):
        conf, decls, pkg = self.conf, self.decls, self.pkg
        viol = []
        ok = []

        def one(p, r):
            fd, td = decls[r["from_decl"]], decls[r["to_decl"]]
            q = pkg[td["id"]]
            if p == q:
                return
            if td["test"] and (td["id"] in conf.testcopy.get(p, ()) or self.root[td["id"]] in conf.testcopy.get(p, ())):
                return
            cls = ""
            if td["test"] and q not in conf.helper:
                cls = "other-package-test-code"
            elif p in conf.helper and q not in conf.allow[p]:
                cls = "helper-import-not-allowed"
            elif p not in conf.helper and p in conf.reach[q]:
                cls = "import-cycle"
            elif not self.exported_end(r) and not td["test"]:
                cls = "unexported-production-symbol"
            if cls:
                viol.append((cls, p, q, r["from_decl"], fd["file"], r["symbol"], r["sites"]))
            else:
                ok.append((p, q, r, td["test"]))

        for r in self.refs:
            fd = decls[r["from_decl"]]
            one(pkg[fd["id"]], r)
            fr = self.root[fd["id"]]
            for p, m in conf.testcopy.items():
                if p != pkg[fd["id"]] and (fd["id"] in m or fr in m):
                    one(p, r)
        return viol, ok


def main():
    out, confpath, mode = sys.argv[1], sys.argv[2], sys.argv[3]
    args = sys.argv[4:]
    conf = Conf(confpath)
    if not hasattr(conf, "reach"):
        conf.reach = {p: set() for p in conf.order}
        for p in conf.order:
            seen, stack = set(), list(conf.allow[p])
            while stack:
                q = stack.pop()
                if q not in seen:
                    seen.add(q)
                    stack.extend(conf.allow[q])
            conf.reach[p] = seen
    m = Model(out, conf)
    decls = m.decls
    viol, ok = m.check()
    if mode == "summary":
        c = collections.Counter()
        s = collections.Counter()
        for cls, p, q, fd, ff, sym, sites in viol:
            c[(cls, p, q)] += 1
            s[cls] += 1
        print(f"illegal test references: {len(viol)}")
        for k, n in sorted(s.items()):
            print(f"  {k:32} {n}")
        for (cls, p, q), n in sorted(c.items()):
            print(f"    {cls:30} {p:14} -> {q:14} {n}")
        if conf.auto and m.unplaced:
            print(f"shared roots with no helper package able to hold them: {len(m.unplaced)}")
            for r, us in sorted(m.unplaced.items()):
                print(f"    {r}\t{decls[r]['file']}\tneed={sorted(m.need(r))}\tusers={dict(us)}")
    elif mode == "syms":
        want = args + [None] * 3
        g = collections.defaultdict(lambda: [0, 0, set(), set()])
        for cls, p, q, fd, ff, sym, sites in viol:
            if want[0] and want[0] != "-" and cls != want[0]:
                continue
            if want[1] and want[1] != "-" and p != want[1]:
                continue
            if want[2] and want[2] != "-" and q != want[2]:
                continue
            e = g[(cls, p, q, sym)]
            e[0] += 1
            e[1] += sites
            e[2].add(ff)
            e[3].add(fd)
        for (cls, p, q, sym), e in sorted(g.items(), key=lambda kv: (kv[0][0], kv[0][1], kv[0][2], -kv[1][1])):
            fl = sorted(e[2])
            print(f"{cls}\t{p}->{q}\t{sym}\trefs={e[0]} sites={e[1]} files={len(fl)}\t{' '.join(fl[:4])}{' ...' if len(fl) > 4 else ''}")
    elif mode == "files":
        g = collections.defaultdict(collections.Counter)
        for cls, p, q, fd, ff, sym, sites in viol:
            g[ff][f"{cls}:{p}->{q}"] += 1
        for ff in sorted(g):
            print(f"{ff}\thome={m.file_home(ff)}\t" + "  ".join(f"{k}={n}" for k, n in g[ff].most_common()))
    elif mode == "file":
        for cls, p, q, fd, ff, sym, sites in sorted(viol):
            if ff == args[0]:
                print(f"{cls}\t{p}->{q}\t{fd}\t{sym}\t{sites}")
    elif mode == "suggest":
        # for each entry point: legal homes and their seam cost
        byfile = collections.defaultdict(list)
        for i, d in decls.items():
            if not is_entry(d):
                continue
            needs = collections.defaultdict(set)  # q -> unexported syms
            pk = set()
            for x in m.closure(i):
                for q, sym, exp, sites, to in m.prodrefs.get(x, ()):
                    pk.add(q)
                    if not (exp or sym in conf.testexport):
                        needs[q].add(sym)
            legal = [h for h in PROD_ORDER if all(q == h or h not in conf.reach[q] for q in pk)]
            cur = m.pkg[i]

            def cost(h):
                return sum(len(v) for q, v in needs.items() if q != h)

            best = min(legal, key=lambda h: (cost(h), h != cur, PROD_ORDER.index(h)))
            byfile[d["file"]].append((i, cur, best, cost(cur) if cur in legal else -1, cost(best), d["loc"],
                                      {q: sorted(v) for q, v in needs.items() if q != best}))
        for ff in sorted(byfile):
            rows = byfile[ff]
            dist = collections.Counter(b for _, _, b, _, _, _, _ in rows)
            home = m.file_home(ff)
            if len(dist) == 1 and home in dist and all(c == 0 for _, _, _, _, c, _, _ in rows):
                continue
            print(f"{ff}\thome={home}\t" + " ".join(f"{p}={n}" for p, n in dist.most_common()))
            if "-v" in args:
                for i, cur, best, ccur, cbest, loc, seams in sorted(rows):
                    if best != cur or cbest:
                        print(f"    {i}\t{cur}->{best}\tcost {ccur}->{cbest}\t{seams if cbest else ''}")
    elif mode == "shared":
        users = m.users_of_roots(m.pkg)
        for root in sorted(users, key=lambda r: (decls[r]["file"], r)):
            us = users[root]
            if len(us) < 2 or is_entry(decls[root]):
                continue
            unexp = collections.defaultdict(set)
            for x in m.closure(root):
                for q, sym, exp, sites, to in m.prodrefs.get(x, ()):
                    if not (exp or sym in conf.testexport):
                        unexp[q].add(sym)
            print(f"{decls[root]['file']}\t{root}\t{decls[root]['kind']}\tpkg={m.pkg[root]}\tneed={','.join(sorted(m.need(root), key=PROD_ORDER.index))}\t"
                  + " ".join(f"{p}={n}" for p, n in sorted(us.items())) + "\tunexp: "
                  + " ".join(f"{q}:{','.join(sorted(s))}" for q, s in sorted(unexp.items())))
    elif mode == "homes":
        # entry points per (file, home) and those that left their file's home
        by = collections.defaultdict(collections.Counter)
        for i, d in decls.items():
            if is_entry(d):
                by[d["file"]][m.pkg[i]] += 1
        for ff in sorted(by):
            if len(by[ff]) > 1 or "-a" in args or m.file_home(ff) != m.filedef[ff]:
                print(f"{ff}\tfamily={m.filedef[ff]}\thome={m.file_home(ff)}\t" + " ".join(f"{p}={n}" for p, n in by[ff].most_common()))
                if "-v" in args:
                    for i, d in sorted(decls.items()):
                        if is_entry(d) and d["file"] == ff and m.pkg[i] != m.file_home(ff):
                            print(f"    {i}\t-> {m.pkg[i]}\t{m.entry_log.get(i, '')}")
    elif mode == "copies":
        tot = collections.Counter()
        for p in sorted(conf.testcopy):
            for r in sorted(conf.testcopy[p]):
                loc = sum(d["loc"] for d in decls.values() if d["test"] and m.root[d["id"]] == r)
                tot[p] += loc
                print(f"{p}\t{r}\t{decls[r]['kind']}\t{decls[r]['file']}\tin={m.pkg[r]}\tloc={loc}")
        print(dict(tot))
    elif mode == "helpers":
        tot = collections.defaultdict(lambda: [0, 0])
        for i, d in sorted(decls.items(), key=lambda kv: (m.pkg[kv[0]], kv[1]["file"], kv[0])):
            if d["test"] and m.pkg[i] in conf.helper:
                tot[m.pkg[i]][0] += 1
                tot[m.pkg[i]][1] += d["loc"]
                if "-v" in args and m.root[i] == i:
                    print(f"{m.pkg[i]}\t{i}\t{d['kind']}\t{d['file']}\t{d['loc']}")
        for p, (n, loc) in tot.items():
            print(f"{p}\tdecls={n}\tloc={loc}")
    elif mode == "pkgs":
        tot = collections.defaultdict(lambda: [0, 0, 0])
        for i, d in decls.items():
            if d["test"]:
                t = tot[m.pkg[i]]
                t[0] += 1
                t[1] += d["loc"]
                t[2] += 1 if is_entry(d) else 0
        for p in conf.order:
            if p in tot:
                print(f"{p:16} test decls={tot[p][0]:5} loc={tot[p][1]:6} entry points={tot[p][2]}")
    elif mode == "emit":
        by = collections.defaultdict(list)
        for i, d in decls.items():
            if d["test"] and m.how.get(i) == "testdecl":
                by[m.pkg[i]].append(i)
        for p in sorted(by):
            ids = sorted(by[p])
            for k in range(0, len(ids), 8):
                print(f"testdecl {p} " + " ".join(ids[k:k + 8]))
        for p in sorted(conf.testcopy):
            ids = sorted(conf.testcopy[p])
            for k in range(0, len(ids), 8):
                print(f"testcopy {p} " + " ".join(ids[k:k + 8]))


if __name__ == "__main__":
    main()
