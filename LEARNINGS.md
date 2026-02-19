# Learnings — Institutional Knowledge Base

Knowledge captured during implementation. Architecture-independent patterns and gotchas carried forward from Phases 1-3 archive.

---

## 1. QuickXorHash (`pkg/quickxorhash/`)

**Plan test vectors were wrong.** The test vectors specified in the plan were incorrect. Verified against rclone v1.73.1's `quickxorhash` package, which is verified against Microsoft's reference C# implementation. Lesson: always verify test vectors against a known-good reference, don't blindly trust specs.

---

## 2. Config TOML Gotchas

- **Chunk size default "10MiB" not "10MB".** 10 MB (decimal, 10,000,000 bytes) is NOT a multiple of 320 KiB (327,680 bytes). 10 MiB (10,485,760) IS a multiple (32 * 327,680). Default was changed to "10MiB" to maintain alignment validation.
- **`toml.MetaData` is 96 bytes** (triggers hugeParam). Must be passed by pointer. `toml.DecodeFile` returns it by value, so take its address when passing to helper functions.
- **misspell linter catches intentional typos in test TOML strings.** Test data for unknown-key-detection uses deliberate misspellings. Must use `//nolint:misspell` on those lines. Inline TOML strings (not raw string literals with backticks) make the nolint placement cleaner.

---

## 3. Agent Coordination

### Parallel agent execution
Four agents ran simultaneously without conflicts (true leaf packages). Scoped verification commands to own package to avoid cross-agent interference:
```bash
# Good: scoped to own package
go test ./internal/graph/...
# Bad: tests all packages, sees intermediate states from other agents
go test ./...
```

---

## 4. Tier 1 Research

16 research documents in `docs/tier1-research/` covering Graph API bugs, reference implementation analysis, and tool surveys. Consult these before implementing any API interaction — they contain critical gotchas (upload session resume, delta headers, hash fallbacks, etc.) tracked as B-015 through B-023 in BACKLOG.md.

