#!/usr/bin/env python3
"""Sizes of what entered a Claude Code session's context, per compaction cycle.

Usage: ctxstat.py TRANSCRIPT.jsonl [CYCLES]
       ctxstat.py TRANSCRIPT.jsonl --find NEEDLE

Prints sizes and shapes only, never content.
"""
import collections
import json
import sys


def blocks(content):
    if isinstance(content, str):
        return [{"type": "text", "text": content}]
    return content or []


def size(value):
    if isinstance(value, str):
        return len(value.encode())
    return len(json.dumps(value, ensure_ascii=False).encode())


def bash_key(command):
    for needle, key in (
        ("runq.py", "Bash runq"),
        ("heads.py", "Bash notes-read"),
        ("cutgrep.py", "Bash notes-read"),
        ("ctx.py", "Bash notes-read"),
    ):
        if needle in command:
            return key
    parts = command.split()
    if not parts:
        return "Bash"
    if parts[0] == "git":
        i = 1
        while i < len(parts) and parts[i].startswith("-"):
            i += 2 if parts[i] == "-C" else 1
        return "Bash git " + (parts[i] if i < len(parts) else "")
    return "Bash " + parts[0].rsplit("/", 1)[-1]


def new_cycle():
    return {
        "first": None, "last": None, "pre": None,
        "by": collections.Counter(), "results": collections.Counter(),
        "calls": collections.Counter(), "inputs": collections.Counter(),
        "base": None, "peak": None, "out": 0, "seen": set(),
        "count": collections.Counter(),
    }


def find(path, needle):
    shown = 0
    with open(path, errors="replace") as handle:
        for number, line in enumerate(handle, 1):
            if needle not in line:
                continue
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            content = (entry.get("message") or {}).get("content")
            kind = "str" if isinstance(content, str) else [b.get("type") for b in blocks(content)]
            attachment = entry.get("attachment") or {}
            print(number, entry.get("type"), entry.get("subtype"), entry.get("timestamp"),
                  "keys=" + ",".join(sorted(entry.keys())),
                  "content=" + str(kind)[:80],
                  "attachment=" + str(attachment.get("type")) + ":" + ",".join(sorted(attachment.keys()))[:120])
            shown += 1
            if shown == 8:
                break


def snapshot(path):
    """Sizes of the parts of the last recorded system prompt and tool list."""
    last = None
    with open(path, errors="replace") as handle:
        for line in handle:
            if '"prompt_snapshot"' not in line:
                continue
            try:
                attachment = json.loads(line).get("attachment") or {}
            except ValueError:
                continue
            if attachment.get("type") == "prompt_snapshot" and "inlineTools" in attachment:
                last = attachment
    for key, value in sorted(last.items(), key=lambda pair: -size(pair[1])):
        print(f"{key}: {type(value).__name__} {size(value)} bytes")
    tools = last.get("tools") or []
    if isinstance(tools, str):
        try:
            tools = json.loads(tools)
        except ValueError:
            print("inlineTools is text, not JSON")
            return
    if isinstance(tools, dict):
        pairs = list(tools.items())
    else:
        pairs = [(str(t.get("name", "?")) if isinstance(t, dict) else "?", t) for t in tools]
    groups, counts = collections.Counter(), collections.Counter()
    for name, value in pairs:
        group = "mcp " + name.split("__")[1] if name.startswith("mcp__") else name
        groups[group] += size(value)
        counts[group] += 1
    for group, total in groups.most_common(14):
        print(f"  {group}: {total} bytes in {counts[group]} tools")


def main():
    path = sys.argv[1]
    if len(sys.argv) > 2 and sys.argv[2] == "--snapshot":
        snapshot(path)
        return
    if len(sys.argv) > 3 and sys.argv[2] == "--find":
        find(path, sys.argv[3])
        return
    want = int(sys.argv[2]) if len(sys.argv) > 2 else 3
    cycles = [new_cycle()]
    names = {}
    with open(path, errors="replace") as handle:
        for line in handle:
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            if entry.get("isSidechain"):
                continue
            cur = cycles[-1]
            stamp = entry.get("timestamp")
            if stamp:
                cur["first"] = cur["first"] or stamp
                cur["last"] = stamp
            kind = entry.get("type")
            if kind == "system" and entry.get("subtype") == "compact_boundary":
                cur["pre"] = (entry.get("compactMetadata") or {}).get("preTokens")
                cycles.append(new_cycle())
            elif kind == "assistant":
                message = entry.get("message") or {}
                usage = message.get("usage") or {}
                total = sum(usage.get(k) or 0 for k in (
                    "input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"))
                if total and message.get("id") not in cur["seen"]:
                    cur["seen"].add(message.get("id"))
                    cur["base"] = cur["base"] or total
                    cur["peak"] = total
                    cur["out"] += usage.get("output_tokens") or 0
                for block in blocks((entry.get("message") or {}).get("content")):
                    btype = block.get("type")
                    if btype == "text":
                        cur["by"]["my text"] += size(block.get("text", ""))
                    elif btype == "thinking":
                        cur["by"]["my thinking"] += size(block.get("thinking", ""))
                    elif btype == "tool_use":
                        name = block.get("name")
                        given = block.get("input") or {}
                        key = bash_key(given.get("command", "")) if name == "Bash" else name
                        names[block.get("id")] = key
                        cur["inputs"][key] += size(given)
            elif kind == "user":
                for block in blocks((entry.get("message") or {}).get("content")):
                    btype = block.get("type")
                    if btype == "tool_result":
                        key = names.get(block.get("tool_use_id"), "?")
                        cur["results"][key] += size(block.get("content", ""))
                        cur["calls"][key] += 1
                    elif btype == "text":
                        key = "compact summary" if entry.get("isCompactSummary") else "user and notices"
                        cur["by"][key] += size(block.get("text", ""))
                    else:
                        cur["by"]["user other"] += size(block)
            elif kind == "attachment":
                attachment = entry.get("attachment") or {}
                cur["by"]["attached " + str(attachment.get("type"))] += size(attachment)
                cur["count"]["attached " + str(attachment.get("type"))] += 1

    def kb(count):
        return f"{count / 1024:.0f}K"

    for index, cur in list(enumerate(cycles))[-want:]:
        results = sum(cur["results"].values())
        inputs = sum(cur["inputs"].values())
        print(f"cycle {index}: {cur['first']} to {cur['last']} preTokens={cur['pre']} "
              f"results={kb(results)} in {sum(cur['calls'].values())} calls, my tool inputs={kb(inputs)}")
        print(f"  tokens: first request {cur['base']}, last request {cur['peak']}, "
              f"my output {cur['out']} in {len(cur['seen'])} requests")
        print("  other: " + ", ".join(
            f"{k} {kb(v)}" + (f"/{cur['count'][k]}" if cur["count"][k] else "")
            for k, v in cur["by"].most_common(9)))
        print("  results: " + ", ".join(
            f"{k} {kb(v)}/{cur['calls'][k]}" for k, v in cur["results"].most_common(8)))
        print("  inputs: " + ", ".join(f"{k} {kb(v)}" for k, v in cur["inputs"].most_common(5)))


main()
