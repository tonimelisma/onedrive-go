# Live Probes

This ledger records direct Graph v1.0 probes run against a OneDrive Personal
test account, each answering one question about server behaviour that the
documentation does not settle or gets wrong. Code and specs cite the IDs.
Current policy lives in [graph-api-quirks.md](graph-api-quirks.md); this file
keeps what was asked, what the server answered, and what it decided.

Every probe ran on Personal unless noted. Business and SharePoint are unprobed
for all of them. Re-run a probe before relying on it for a new decision: the
server's behaviour can drift.

## 2026-10-25, first batch (unnumbered, cited as probes 1-8)

| # | Question | Result | Decided |
|---|---|---|---|
| 1 | Are names with `%` or `+` returned encoded (`a%20b.txt`, `test%46.txt`, `x%2Fy.txt`, `plus+sign.txt`, `pct%.txt`)? | No. The create answer, `GET` by ID, `/children` and `/delta` all return the literal name. | Names are opaque; the client never URL-decodes them (D-020). |
| 2 | Are NFC and NFD spellings one name (`café.txt`)? | Yes. Uploading the NFD name into the same folder returned the NFC item's ID and replaced its content. | Unicode-form twins are true collisions; name keys normalize (D-024). |
| 3 | Does case folding go beyond ASCII (`straße`/`STRASSE`, `i̇stanbul`/`İstanbul`, `σ`/`Σ`)? | All collide (409 with `conflictBehavior=fail`). | `strings.EqualFold`/`ToLower` are not OneDrive's comparison (D-024); see L-33. |
| 4 | Is it safe to put a user name in a path-routed URL (`items/{parent}:/{name}:/content`)? | Most names are fine, but `v1.0` made Graph create a folder `_api` holding a file `v2.0`. | New files are created by body, content goes by item ID (D-021). |
| 5 | Does a `fileSystemInfo` PATCH or a rename silently no-op? | No: 200 with JSON, change applied. | Mutation echoes are still checked, but no account-wide workaround. |
| 6 | What mtime precision is stored? | Whole seconds, truncated (.499, .500 and .999 become .000). | Matches the comparable-mtime rule. |
| 7 | How many versions does a simple upload plus mtime PATCH create? | Two, on Personal too. | Non-empty files go through upload sessions carrying `fileSystemInfo`. |
| 8 | Does `permanentDelete` work on Personal? | Yes, 204. | The earlier "Personal returns 405" note was stale. |

## 2026-10-25, second batch (L-01 to L-30)

| ID | Question | Result | Decided |
|---|---|---|---|
| L-01 | Create by JSON body, then content by ID | `POST /items/{parent}/children {name, file:{}, conflictBehavior:fail}` gives 201 and a 0-byte item already carrying QuickXor `AAAA…=`. A session on `/items/{id}` with `fileSystemInfo` and 1 KiB of content still leaves one version, same ID. An existing name or case variant with `fail` gives 409 `nameAlreadyExists`. | Create by body, content by ID (D-021). |
| L-02 | If-Match with a stale eTag | 412 for `PUT /items/{id}/content`, `createUploadSession`, `PATCH` and `DELETE`. A session opened with a fresh If-Match whose item then changes answers its next fragment with 404 "upload session not found". | If-Match on every overwrite, move and delete (D-022); that 404 is an invalidation, not a blind restart. |
| L-03 | Extra fold cases | `ǅ`/`ǆ` collide; `ﬁle`/`FILE` collide. Kelvin `K` vs `K`, U+732A vs U+FA16, and `µ` vs `Μ` do not. | Full case folding over-collides; the key is an explicit table (see L-33). |
| L-04 | Are NFD names stored as NFD, and can they be renamed to NFC? | Stored as NFD bytes; a PATCH to the NFC spelling answers 409 (it collides with itself). | Keep the remote spelling, map by key, never "fix" spelling remotely (D-027). |
| L-05 | Name length | Path-routed uploads fail with 400 `maxUrlLength` from about 292 characters: the limit is the URL. By body, 300 ASCII characters succeed and 400 fail ("400 characters or less" per path and per name). Emoji names up to 260 UTF-16 units succeed. | Create by body; the length rule counts UTF-16 units (D-029). |
| L-06 | C1 controls, `~`, trailing space, NBSP | All accepted on Personal (`\x85`, `\x9f`, `~draft.txt`, `~$lock.docx`, `trail .txt`, NBSP, folder `~notes`). | Do not reject these on Personal; Business rules stay gated by drive type (D-032). |
| L-09 / L-10 | Modification time across rename and move | A rename resets both top-level and `fileSystemInfo.lastModifiedDateTime` to now; a later move did not change it again; `createdDateTime` is unchanged. A PATCH carrying `name` or `parentReference` together with `fileSystemInfo` keeps the sent time. Renaming a parent leaves a child's time alone (the child's eTag changes, its cTag does not). | Every move or rename carries the item's current time (D-150, R-2.17.5, R-1.7.4). |
| L-11 | `parentReference: {id: "root"}` | Works; the parent becomes the real root ID. | No need to cache the root ID for moves. |
| L-13 | Restore from the recycle bin | `POST /items/{id}/restore` gives 200, the same ID and a new eTag. | A restored item comes back with its old ID (D-061). |
| L-16 | Move or rename onto an existing name without `conflictBehavior` | 409 `nameAlreadyExists`, also for a case variant. A case-only rename of an item itself gives 200. | No silent replace on move; 409 is handled (D-058). |
| L-17 | Hashes of empty files | QuickXor `AAAAAAAAAAAAAAAAAAAAAAAAAAA=` and a download URL are present. | Empty files have a hash on Personal. |
| L-20 | Where does `/content` redirect? | 302 to `my.microsoftpersonalcontent.com`, which serves without the bearer token. | Never forward the bearer to the redirect host (D-086). |
| L-26 | Timestamp range | The earliest accepted `lastModifiedDateTime` is 1900-01-01T00:00:00Z; earlier dates answer a misleading 404 `itemNotFound`. 9999-12-31 is accepted. 0001-01-01 is silently replaced by now. | Clamp sent times to 1900, never send the zero time, and read that 404 as a range error (D-046). |
| L-30 | Characters outside the Basic Multilingual Plane | Ignored when comparing names: `😀.txt` = `😀😀.txt` = `😁.txt`, `a😀.txt` = `a.txt`, `𠀀.txt` = `𠀁.txt`, `👨‍👩.txt` = `👨.txt`. BMP symbols (`☃`/`☄`), ZWSP and the soft hyphen are compared normally. Stored names keep the characters. | The name key drops code points above U+FFFF (D-151). |

## 2026-10-26, third batch (L-32 to L-35, M-1)

| ID | Question | Result | Decided |
|---|---|---|---|
| L-32 | Is `parentReference.path` URL-encoded? | No. Folders `a b`, `a%b`, `a%20b`, `a#b`, `ä`, `a+b`, `a&b` and `a'b` appear verbatim in their children's path, in the create answer, `GET` and delta. | Keep the path verbatim; locate items by parent ID plus name (R-6.7.23). |
| L-33 | Which spellings does OneDrive treat as one name? | Same: U+0958 vs U+0915 U+093C, ς vs σ, `x` vs `x` + U+FE0F. Different: U+FB2A vs U+05E9 U+05C1, U+1F71 vs U+03AC, ſ vs s, ϐ vs β, ZWNJ, ZWSP, soft hyphen. | Some canonically equivalent siblings are distinct items, so this client holds them as remote twins (R-2.12.6). |
| M-1 | Systematic name-equivalence probe | 592 pairs, sent one at a time: every BMP code point whose full case folding differs from simple lowercasing, every composition exclusion, the canonical singletons and the default-ignorable format characters. The 506 pairs in `internal/namekey/testdata/probe-m1-personal.tsv` are the recorded results. | `internal/namekey` encodes the result as explicit tables, and its tests replay every pair (R-2.12.3). |
| L-34 | What does a path-routed upload session leave before it commits? | A visible 0-byte item at once, QuickXor `AAAA…`, eTag `",1"`; the commit keeps the ID and moves the eTag to `",2"`. Another writer's case-variant create during the session gets 409; a session for an existing name with `fail` answers 409 at creation. | An interrupted upload can leave an empty item; sync records a pending create and treats that item as its own (R-5.6.15). |
| L-35 | Name length through path-routed sessions | 400 for names of 300 characters or more; 250 works. | One more reason new files are created by body. |
