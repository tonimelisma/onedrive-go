# Tier 1 Research Documents

## Provenance

These documents were produced using a clean room methodology. Research agents studied an existing open-source OneDrive sync client and the Microsoft Graph API documentation, then documented observable behaviors, API quirks, and edge cases **in their own words**.

These documents describe **facts about API behavior and publicly-known bugs** — not source code structure, internal algorithms, or copyrightable expression. Primary sources include:

- Microsoft Graph API official documentation
- Public GitHub issue trackers
- Observed API response behavior

No third-party source code is quoted or reproduced in these documents. The onedrive-go project (MIT licensed) uses these documents as domain research only. All code in this repository is original work.

## Documents

| Document | Description |
|----------|-------------|
| `domain-glossary.md` | OneDrive/Graph shared vocabulary |
| `api-analysis.md` | Graph API endpoint analysis |
| `api-item-field-matrix.md` | DriveItem field availability per account type |
| `ref-sync-algorithm.md` | Reference sync engine observable behavior |
| `ref-conflict-scenarios.md` | Conflict handling patterns |
| `ref-filtering-rules.md` | Filter types and semantics |
| `ref-config-inventory.md` | Config options catalog |
| `ref-edge-cases.md` | Edge cases, gotchas, hard-won lessons |
| `issues-graph-api-bugs.md` | 12 Graph API bugs with workarounds |
| `issues-api-inconsistencies.md` | 26 API inconsistencies cataloged |
| `issues-feature-requests.md` | User feature requests and needs analysis |
| `issues-common-bugs.md` | Common production bugs and defensive patterns |
| `survey-sync-cli-tools.md` | Survey of 14 file sync/backup CLI tools |
| `survey-gui-ipc-patterns.md` | GUI/IPC integration patterns for sync daemons |
| `survey-sync-state-models.md` | Sync state DB models (6 tools surveyed) |
