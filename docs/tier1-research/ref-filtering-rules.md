# Reference Implementation: File and Folder Filtering Rules

This document analyzes how the reference OneDrive sync client handles file and folder filtering. It describes the observable behaviors, semantics, configuration surface, and edge cases of the filtering system. This is a Tier 1 research document intended to inform our own implementation design.

---

## Table of Contents

1. [Overview](#overview)
2. [Filter Types](#filter-types)
   - [skip_file](#skip_file)
   - [skip_dir](#skip_dir)
   - [skip_dir_strict_match](#skip_dir_strict_match)
   - [skip_dotfiles](#skip_dotfiles)
   - [skip_symlinks](#skip_symlinks)
   - [skip_size](#skip_size)
   - [sync_list](#sync_list)
   - [check_nosync](#check_nosync)
   - [sync_root_files](#sync_root_files)
3. [Name Validation and Invalid Character Handling](#name-validation-and-invalid-character-handling)
   - [Disallowed Names](#disallowed-names)
   - [Invalid Characters](#invalid-characters)
   - [Bad Whitespace](#bad-whitespace)
   - [ASCII HTML Codes](#ascii-html-codes)
   - [ASCII Control Codes](#ascii-control-codes)
   - [UTF-16 Validation](#utf-16-validation)
   - [Character Encoding Validation](#character-encoding-validation)
4. [Filter Evaluation Order](#filter-evaluation-order)
   - [Upload Path (Local to Remote)](#upload-path-local-to-remote)
   - [Download Path (Remote to Local)](#download-path-remote-to-local)
5. [Sync Direction Filtering](#sync-direction-filtering)
6. [sync_list Deep Dive](#sync_list-deep-dive)
   - [File Format](#file-format)
   - [Rule Types](#rule-types)
   - [Matching Semantics](#matching-semantics)
   - [Interaction with Other Filters](#interaction-with-other-filters)
7. [Business Shared Folders Filtering](#business-shared-folders-filtering)
8. [Path Length Limits](#path-length-limits)
9. [Wild2regex: Pattern Compilation](#wild2regex-pattern-compilation)
10. [Shadow Validation](#shadow-validation)
11. [Design Observations and Potential Issues](#design-observations-and-potential-issues)

---

## Overview

The reference implementation uses a module called `clientSideFiltering` that centralizes most filtering logic. However, significant additional filtering occurs in the main sync engine (`sync.d`) and utility module (`util.d`). Filtering is applied to both uploads and downloads, though the evaluation paths differ slightly between directions.

All filtering is client-side. There is no server-side filtering configuration. The client decides what to skip before making any API calls for the skipped item.

The filtering system can be broadly divided into two categories:

1. **User-configurable filters** -- Rules the user sets via configuration files or CLI options (skip_file, skip_dir, skip_dotfiles, skip_symlinks, skip_size, sync_list, check_nosync).
2. **Hardcoded validation rules** -- Checks for names and characters that OneDrive does not support (invalid characters, reserved names, control codes, encoding issues). These are always applied and cannot be overridden by the user.

---

## Filter Types

### skip_file

**Purpose:** Exclude specific files from sync operations based on filename patterns.

**Configuration:**
- Config file: `skip_file = "pattern1|pattern2|pattern3"`
- CLI: `--skip-file 'pattern1|pattern2|...'`
- Multiple `skip_file` lines in the config file are concatenated with `|` separators.
- If the CLI option is used, it replaces (not appends to) the config file value.

**Default value:** `~*|.~*|*.tmp|*.swp|*.partial`

The defaults skip:
- `~*` -- Files starting with `~` (editor backup/temp files)
- `.~*` -- Files starting with `.~` (LibreOffice/OpenOffice lock files)
- `*.tmp` -- Generic temporary files
- `*.swp` -- Vim swap files
- `*.partial` -- Partially downloaded files

**Pattern syntax:** Wildcard patterns using `*` (any characters) and `?` (single character), separated by `|`. These are converted to regular expressions via the `wild2regex` function (see dedicated section below).

**Case sensitivity:** Case-insensitive. The `wild2regex` function passes the `"i"` flag when compiling the regex.

**Matching scope:** The filter tries two matches:
1. The full relative path (e.g., `Documents/file.tmp`)
2. The basename only (e.g., `file.tmp`)

If either matches, the file is excluded. This means a skip_file pattern like `*.tmp` will match regardless of where in the directory tree the file lives.

**Applies to:** Files only. Not applied to directories. Applied in both upload and download directions.

**Important:** If you override the default `skip_file` value, the built-in defaults are lost. Users must re-include them explicitly if they still want the default temporary file exclusions.

---

### skip_dir

**Purpose:** Exclude specific directories from sync operations based on directory name patterns.

**Configuration:**
- Config file: `skip_dir = "pattern1|pattern2|pattern3"`
- CLI: `--skip-dir 'pattern1|pattern2|...'`
- Multiple `skip_dir` lines in the config file are concatenated with `|` separators.
- If the CLI option is used, it replaces the config file value.

**Default value:** Empty (no directories skipped by default).

**Pattern syntax:** Same wildcard-to-regex conversion as skip_file (`wild2regex`). Patterns separated by `|`.

**Case sensitivity:** Case-insensitive (due to `wild2regex` using the `"i"` flag).

**Matching behavior (non-strict mode, which is the default):**

The directory name check is surprisingly complex. Given an input path, the reference:

1. Normalizes the path by stripping a leading `./` if present.
2. Builds a set of candidate paths including the original path, the path with and without a leading `/`, and each of those with and without a trailing `/`.
3. Tests all candidates against the compiled `directoryMask` regex for a full-path match.
4. If no full-path match is found, and strict matching is NOT enabled, it splits the path into individual segments and tests each segment independently against the regex. This means a skip_dir rule of `temp` will match a directory named `temp` at any depth in the tree (e.g., `Documents/Projects/temp` would match because the segment `temp` matches).

**Matching behavior (strict mode):**

When `skip_dir_strict_match` is enabled, only the full-path match (step 3 above) is performed. Individual path segment matching is skipped. This means a rule must match the entire relative path to exclude a directory.

**Validation:** The reference rejects the pattern `.*` (literal dot-star) as a skip_dir entry, directing users to use `skip_dotfiles` instead. This is because `.*` would match everything and prevent correct operation.

**Applies to:** Directories only. Applied in both upload and download directions.

---

### skip_dir_strict_match

**Purpose:** Changes the skip_dir matching behavior from segment-based (match any path component) to full-path-based (must match the entire relative path).

**Configuration:**
- Config file: `skip_dir_strict_match = "true"`
- CLI: `--skip-dir-strict-match`

**Default value:** False (non-strict; segment matching is enabled by default).

**Behavior when disabled (default):** A skip_dir pattern like `temp` will exclude any directory named `temp` anywhere in the tree.

**Behavior when enabled:** A skip_dir pattern like `temp` will only match a top-level directory called `temp`. To exclude `Documents/temp`, you would need the pattern `Documents/temp` or `/Documents/temp`.

---

### skip_dotfiles

**Purpose:** Skip all files and folders whose name begins with a `.` (dot).

**Configuration:**
- Config file: `skip_dotfiles = "true"`
- CLI: `--skip-dot-files`

**Default value:** False.

**Matching behavior:** The reference extracts the last component of the path (the basename) and checks if it starts with `.`. The root path `.` is explicitly excluded from this check (it is never treated as a dotfile).

**Interaction with sync_list:** When both `skip_dotfiles` and `sync_list` are active, the behavior differs between upload and download paths:
- On the upload path, if `skip_dotfiles` is enabled and the item is a dotfile, but `sync_list` is also configured, the item is marked as "potentially skipping" rather than immediately excluded. This suggests that `sync_list` inclusion can override `skip_dotfiles` in some cases for uploads.
- On the download path, `skip_dotfiles` is checked after `sync_list` and independently skips items without consulting `sync_list`.

**Applies to:** Both files and directories. Applied in both upload and download directions.

---

### skip_symlinks

**Purpose:** Skip all symbolic links during sync operations.

**Configuration:**
- Config file: `skip_symlinks = "true"`
- CLI: `--skip-symlinks`

**Default value:** False.

**Behavior:** When enabled, any path that is a symbolic link is skipped. When disabled, symbolic links are still subject to validation:
- If a symlink target does not exist (broken symlink), it is always skipped with a warning, regardless of the `skip_symlinks` setting.
- The reference attempts to resolve relative symlinks by changing to the parent directory of the link and re-reading the link target. If the relative target resolves successfully, the symlink is kept.
- Only applies during local filesystem scanning (upload direction). There is no concept of symlinks in the OneDrive API, so this filter does not apply on the download path.

**Applies to:** Both files and directories that are symbolic links. Upload direction only.

---

### skip_size

**Purpose:** Skip files larger than a specified size threshold.

**Configuration:**
- Config file: `skip_size = "50"` (value in MB)
- CLI: `--skip-size '50'`

**Default value:** 0 (disabled; all files synced regardless of size).

**Behavior:** The configured MB value is converted to bytes (multiplied by 2^20). Files whose size is greater than or equal to this limit are skipped. A value of 0 means no limit.

**Applies to:** Files only (size is not meaningful for directories). Applied in both upload and download directions. On the download path, the file size is read from the OneDrive API JSON response. On the upload path, the file size is read from the local filesystem.

---

### sync_list

**Purpose:** Provide fine-grained selective synchronization by specifying exactly which paths should be included (and optionally which should be excluded from those inclusions).

**Configuration:** A file named `sync_list` placed in the application configuration directory (default: `~/.config/onedrive/sync_list`). Not a config file option or CLI option.

**Default behavior when absent:** When no `sync_list` file exists, everything is included (no selective filtering).

**Default behavior when present:** When a `sync_list` file exists and contains rules, EVERYTHING is excluded by default. Only paths matching inclusion rules are synced.

This is covered in detail in the [sync_list Deep Dive](#sync_list-deep-dive) section.

---

### check_nosync

**Purpose:** Skip a directory if it contains a `.nosync` marker file.

**Configuration:**
- Config file: `check_nosync = "true"`
- CLI: `--check-for-nosync`

**Default value:** False.

**Behavior:** When enabled, during local filesystem scanning (upload path), if a directory contains a file named `.nosync`, that directory is skipped. During download, the check looks for `.nosync` in the parent directory of the item being downloaded.

**Important limitation:** This only prevents upload of local directories. It does not check for `.nosync` files online to prevent downloads of directories. The documentation explicitly states this limitation.

---

### sync_root_files

**Purpose:** When `sync_list` is in use, sync all files in the root of the sync directory without requiring explicit `sync_list` rules for each root-level file.

**Configuration:**
- Config file: `sync_root_files = "true"`
- CLI: `--sync-root-files`

**Default value:** False.

**Behavior:** When `sync_list` is active, files in the root of the sync directory would normally be excluded unless explicitly listed. Enabling `sync_root_files` automatically includes any file at the root level, overriding sync_list exclusion for those files. This only applies to files, not directories, and only at the root level (not nested paths).

---

## Name Validation and Invalid Character Handling

Before any user-configurable filter is applied during upload, the reference validates each path against Microsoft OneDrive's naming restrictions. These checks are hardcoded and cannot be disabled. They are performed in the sync engine before client-side filtering rules are evaluated.

### Disallowed Names

The following names are unconditionally rejected (case-insensitive comparison):

| Name | Category |
|------|----------|
| `.lock` | Lock file |
| `desktop.ini` | Windows system file |
| `CON`, `PRN`, `AUX`, `NUL` | DOS reserved device names |
| `COM0` through `COM9` | DOS serial port names |
| `LPT0` through `LPT9` | DOS parallel port names |

Additionally:
- Any name starting with `~$` is rejected (Office temporary files).
- Any name containing `_vti_` is rejected (SharePoint internal directory).
- A folder named `forms` at the root level (first or second path segment) is rejected (SharePoint reserved name). This check is case-insensitive.

### Invalid Characters

The following characters are prohibited in file and folder names on OneDrive. The reference uses a regex to detect them:

| Character | Description |
|-----------|-------------|
| `<` | Less than |
| `>` | Greater than |
| `:` | Colon |
| `"` | Double quote |
| `\|` | Pipe |
| `?` | Question mark |
| `*` | Asterisk |
| `/` | Forward slash |
| `\` | Backslash |

Additionally:
- Leading whitespace (one or more whitespace characters at the start of the name) is rejected.
- Trailing whitespace (a whitespace character at the end of the name) is rejected.
- Trailing dot (`.` at the end of the name) is rejected.

These restrictions come from Microsoft's published OneDrive and SharePoint restrictions. The reference does not differentiate between Personal and Business accounts for these character restrictions -- the same rules are applied universally.

### Bad Whitespace

The reference specifically checks for newline characters embedded in filenames. The check URL-encodes the basename and looks for `%0A` (encoded newline). Files with newlines in their names are skipped because the OneDrive API cannot handle them in path-based queries.

### ASCII HTML Codes

Filenames containing HTML entity sequences (the pattern `&#` followed by 1 to 4 digits and a semicolon, e.g., `&#169;`) are rejected. These cause errors when uploading to OneDrive.

### ASCII Control Codes

Any path containing ASCII control characters (code points 0x00-0x1F, 0x7F) or Unicode control characters (the `Cc` Unicode category) is rejected. This blocks certain non-printable characters that could cause issues.

### UTF-16 Validation

The reference validates that paths can be correctly encoded as UTF-16 (since the OneDrive/Windows filesystem uses UTF-16 internally). Invalid surrogate pairs or invalid code units cause the path to be rejected.

### Character Encoding Validation

On the download path, the reference validates the encoding of paths received from the API against multiple encoding standards (Unicode 5.0, ASCII, ISO-8859-1, ISO-8859-2, WINDOWS-1250, WINDOWS-1251, WINDOWS-1252). Items with invalid encoding sequences are skipped.

---

## Filter Evaluation Order

The documented filter evaluation order is:

1. `check_nosync`
2. `skip_dotfiles`
3. `skip_symlinks`
4. `skip_dir`
5. `skip_file`
6. `sync_list`
7. `skip_size`

However, the actual implementation differs slightly between the upload and download code paths. Additionally, the name validation checks (invalid characters, reserved names, etc.) are applied before any of these filters in the upload path, and selectively on the download path.

### Upload Path (Local to Remote)

When scanning the local filesystem for items to upload, the processing order is:

1. **Name validation** (hardcoded, not user-configurable):
   - Microsoft naming convention check (`isValidName`)
   - Bad whitespace check (`containsBadWhiteSpace`)
   - ASCII HTML code check (`containsASCIIHTMLCodes`)
   - UTF-16 encoding validation (`isValidUTF16`)
   - ASCII control code check (`containsASCIIControlCodes`)
2. **check_nosync** -- Check for `.nosync` marker file
3. **skip_dotfiles** -- Check if item is a dotfile/dotfolder
4. **skip_symlinks** -- Check if item is a symbolic link (plus broken-link handling)
5. **skip_dir** -- Check directory name against skip_dir patterns (directories only)
6. **skip_file** -- Check filename against skip_file patterns (files only)
7. **sync_list** -- Check path against sync_list rules (with `sync_root_files` override)
8. **skip_size** -- Check file size against configured limit (files only)

If any check excludes the item, subsequent checks are short-circuited (not evaluated).

### Download Path (Remote to Local)

When processing items from the OneDrive delta API response, the checks are applied in a different order. The download path has two separate filtering locations in the code:

**Primary download filter (during delta item processing):**
1. **skip_file** -- Applied to the item name from the JSON response
2. **skip_dir** -- Applied to the computed path
3. **sync_list** -- Applied to the computed path (with `sync_root_files` override)
4. **skip_dotfiles** -- Applied to the computed path
5. **check_nosync** -- Check local parent for `.nosync` file
6. **skip_size** -- Check file size from the JSON response

**Secondary download filter (JSON-based client side filtering):**
A separate code path exists that checks JSON items against client-side filtering rules. This path is noted as having some missing checks:
- `check_nosync` -- explicitly marked as MISSING
- `skip_dotfiles` -- explicitly marked as MISSING
- `skip_symlinks` -- explicitly marked as MISSING
- `skip_dir` -- present
- `skip_file` -- present
- `sync_list` -- present
- `skip_size` -- present

This is a notable observation: the secondary filtering path is incomplete and does not check all rules that the primary path does.

**Character encoding validation on download:**
The download path also checks items for valid character encoding sequences using the D standard library's `isValid` function. Items with invalid encoding are skipped.

---

## Sync Direction Filtering

Most filters are applied symmetrically in both upload and download directions, with the following exceptions:

| Filter | Upload | Download | Notes |
|--------|--------|----------|-------|
| skip_file | Yes | Yes | Same patterns, same matching |
| skip_dir | Yes | Yes | Same patterns, same matching |
| skip_dotfiles | Yes | Yes | |
| skip_symlinks | Yes | No | Symlinks are a local filesystem concept |
| skip_size | Yes | Yes | Local size vs. API-reported size |
| sync_list | Yes | Yes | Same rules file, same matching |
| check_nosync | Yes | Partial | Upload checks the directory; download checks the local parent |
| Name validation | Yes | Partial | Full suite on upload; encoding check on download |
| sync_root_files | Yes | Yes | Overrides sync_list exclusion at root |

There is no mechanism to apply different filter rules for upload versus download. All user-configurable filters apply equally in both directions.

---

## sync_list Deep Dive

### File Format

The `sync_list` file is a plain text file with one rule per line.

- Empty lines and whitespace-only lines are ignored.
- Lines beginning with `#` or `;` are comments and are ignored.
- Each non-comment, non-empty line is a rule.
- Paths are normalized using `buildNormalizedPath` before storage.
- Spaces in paths do not need escaping.

**Invalid rules that are rejected:**
- `!/*` or `!/` or `-/*` or `-/` -- These would exclude everything and are treated as errors.
- `/*` or `/` -- These legacy root-include rules are rejected with a message to use `sync_root_files` instead.
- Rules starting with `./` -- These are rejected as malformed.

### Rule Types

Rules fall into two broad categories:

#### Inclusion Rules
- No prefix, or starting with `/`
- Examples: `Documents/`, `/Backup`, `*.pdf`, `Work/Project*`

#### Exclusion Rules
- Prefixed with `!` or `-`
- Examples: `!Documents/temp*`, `!/Secret_data/*`, `-node_modules/*`

Within each category, rules can be:

1. **Rooted rules** (start with `/`): Match only at the specified path relative to the sync root.
   - `/Documents` matches only a `Documents` folder at the root of the sync directory.
   - `!/Secret_data/*` excludes only a `Secret_data` folder at the root.

2. **Anywhere rules** (do NOT start with `/`): Match the pattern anywhere in the directory tree.
   - `Documents/` matches a `Documents` folder at any depth.
   - `!node_modules/*` excludes any `node_modules` folder at any depth.
   - These are the most expensive rules because they require scanning every folder.

3. **Wildcard rules** (contain `*`): Use single-star wildcards for single-segment matching.
   - `Documents/*.pdf` matches PDF files inside any `Documents` folder.
   - `Work/Project*` matches items starting with `Project` inside any `Work` folder.

4. **Globbing rules** (contain `**`): Use double-star for recursive directory matching.
   - `/Programming/Projects/Android/**/build/*` matches `build` directories at any depth under `/Programming/Projects/Android/`.

### Matching Semantics

The sync_list evaluation is complex. For a given input path, the system iterates through all rules and makes a determination:

**Default state:** If sync_list rules exist, the default is to EXCLUDE (i.e., `finalResult = true` means excluded). A path is only included if a rule explicitly matches it for inclusion.

**Rule processing flow for each rule:**

1. Determine if the rule is an exclusion (`!` or `-` prefix) or inclusion (no prefix or `/` prefix).
2. Strip the prefix character if it is `!` or `-`.
3. The rule is categorized and tested in this order:
   a. **Exact match test** (for rules starting with `/` that contain no wildcards): Compare path segments directly.
   b. **Parental path match test**: Check if the input path is a parent of a rule path, or the rule path is a parent of the input path.
   c. **Anywhere match test** (for rules not starting with `/`): Use `canFind` (substring match) and regex matching.
   d. **Wildcard/globbing match test** (for rules containing `*` or `**`): Use regex and segment-by-segment matching.

**Short-circuit behavior:**
- An **inclusion exact match** immediately breaks the loop and includes the path.
- An **exclusion exact match** sets a flag but does NOT break -- remaining rules continue to be evaluated.
- **Anywhere rule matches** (both include and exclude) DO break the loop immediately.
- **Wildcard/globbing matches** do NOT break the loop -- remaining rules continue.

**Final determination:**
After all rules are processed, if ANY exclusion flag is set (`excludeExactMatch`, `excludeParentMatched`, `excludeAnywhereMatched`, `excludeWildcardMatched`, or `exclude`), the path is excluded. This means exclusion rules generally take precedence, but the documentation states the model follows "exclude overrides include" with the recommendation to place exclusions before inclusions.

**Prefix matching for parent paths:**
A separate function (`isSyncListPrefixMatch`) checks whether the input path is a prefix of any inclusion rule. This allows parent directories of included paths to pass through (e.g., if `/Documents/Work` is included, then `/Documents` passes because it is a prefix of an included path). This is critical for the sync engine to be able to traverse the directory tree to reach included items.

**Root path:** The root path `.` is always included (never excluded by sync_list).

### Interaction with Other Filters

- **sync_root_files:** When sync_list would exclude a root-level file, but `sync_root_files` is enabled, the file is included anyway. This override only applies to files (not directories) and only at the root level.
- **skip_dotfiles:** When both are active, on the upload path, dotfile detection defers to sync_list (the item is "potentially skipped" pending sync_list evaluation rather than immediately excluded). On the download path, skip_dotfiles acts independently.
- **Shadow validation:** At initialization, the reference validates that sync_list inclusion rules are not "shadowed" by skip_file or skip_dir entries. If a sync_list inclusion rule would be excluded by skip_file or skip_dir, initialization fails with an error. This prevents configurations where included paths can never actually be synced.

---

## Business Shared Folders Filtering

Shared folders (remote items) receive some filtering, but the treatment differs from normal items:

1. **skip_dir** is applied to shared folder names. If a shared folder's name matches a skip_dir pattern, it is skipped entirely.

2. **sync_list** is applied during delta processing of shared folder contents, just as it is for the user's own drive.

3. For **OneDrive Business** accounts, shared folders are only synced when `sync_business_shared_items` is set to `true`. This is an all-or-nothing toggle -- individual shared folders cannot be selectively enabled/disabled through this mechanism (though skip_dir can exclude specific ones by name).

4. For **OneDrive Personal** accounts, shared folders (remote items) from the database are iterated and each is checked against `skip_dir` before syncing its contents.

5. **skip_file**, **skip_dotfiles**, **skip_size**, and **skip_symlinks** are applied to individual items within shared folders during the normal delta processing flow, just as they would be for any other item.

6. Only remote items of type `dir` (directory) are processed for Business shared folder sync. Remote file items are handled separately via `--sync-shared-files`.

---

## Path Length Limits

The reference implementation does not appear to implement explicit path length validation against OneDrive's documented limits. Microsoft's documented limits are:

- Maximum path length (entire URL-encoded path): 400 characters
- Maximum filename length: 400 characters (effectively limited by the path length)

The reference does not have any code that checks path length or rejects paths that exceed these limits. This means overly long paths would fail at the API level with an HTTP error rather than being caught and reported by the client.

This is a gap in the reference implementation that our implementation should address proactively.

---

## Wild2regex: Pattern Compilation

The `wild2regex` function converts user-specified wildcard patterns (used by skip_file and skip_dir) into D regular expressions. Understanding its behavior is important because it defines the actual matching semantics.

**Conversion rules:**

| Input Character | Regex Output | Meaning |
|-----------------|-------------|---------|
| `*` | `.*` | Match any characters (including path separators) |
| `?` | `.` | Match any single character (including path separators) |
| `.` | `\\.` | Literal dot |
| `\|` | `$\|^` | Pattern separator (creates alternation) |
| `+` | `\\+` | Literal plus |
| ` ` (space) | `\\s` | Match one whitespace character |
| `/` | `\\/` | Literal forward slash |
| `(` | `\\(` | Literal open parenthesis |
| `)` | `\\)` | Literal close parenthesis |
| Other | Passed through | Literal character |

**Anchoring:** The resulting regex is anchored with `^` at the start and `$` at the end.

**Flags:** The regex is compiled with the `"i"` (case-insensitive) flag.

**Key observations:**
- `*` maps to `.*` which crosses path separators. This means `*.tmp` will match `path/to/file.tmp` as well as `file.tmp`. The original implementation used `[^/]*` (which would NOT cross path separators) but was changed.
- Space maps to `\s` (any whitespace), not a literal space. This means a space in a pattern matches any single whitespace character.
- This function is used for both skip_file and skip_dir patterns.
- The sync_list uses a different regex construction function (`createRegexCompatiblePath`) for its wildcard rules, which also maps `*` to `.*` but handles escaping differently.

---

## Shadow Validation

The reference performs two shadow validation checks at startup:

### sync_list shadowed by skip_dir

If any sync_list inclusion rule would be excluded by skip_dir evaluation, initialization fails. The check:
1. Iterates over all sync_list inclusion rules.
2. Normalizes each rule (strips leading `./` and `/`).
3. Passes the normalized rule through the actual `isDirNameExcluded` function.
4. Also tests with a trailing `/` appended.
5. If any inclusion rule is shadowed, all shadowed rules are reported and the application exits.

### sync_list shadowed by skip_file

If any sync_list inclusion rule would be excluded by skip_file evaluation, initialization fails. The check:
1. Iterates over all sync_list inclusion rules.
2. Skips rules that look like directory-only rules (ending with `/`).
3. Normalizes each rule.
4. Passes the normalized rule through the actual `isFileNameExcluded` function.
5. If any inclusion rule is shadowed, all shadowed rules are reported and the application exits.

These validations prevent invalid configurations where the user has conflicting filter rules.

---

## Design Observations and Potential Issues

### Observations for Our Implementation

1. **Case insensitivity is pervasive.** Both skip_file and skip_dir patterns are case-insensitive. The wild2regex function always uses the `"i"` flag. The sync_list anywhere-rule matching uses `canFind` (case-sensitive string search) as a first attempt, then falls back to regex. This inconsistency could lead to subtle matching differences depending on which code path is taken.

2. **The `*` wildcard crosses path separators.** This is a deliberate design choice (changed from the original behavior). It means `*.tmp` in skip_file will match files at any depth, which is probably the desired behavior for most users but differs from traditional glob semantics.

3. **sync_list is the most complex filter.** Its evaluation involves multiple matching strategies (exact, parental, anywhere, wildcard, globbing) and the interaction between inclusion and exclusion rules has subtle ordering effects. The documentation says "exclude overrides include" but the implementation has cases where an inclusion exact-match can break out of evaluation early, potentially before an exclusion rule is seen.

4. **Filter evaluation is not fully consistent between upload and download.** The download path has an acknowledged incomplete secondary filtering function that misses check_nosync, skip_dotfiles, and skip_symlinks checks. The evaluation order also differs between directions.

5. **Name validation is upload-only (mostly).** The full suite of name validation checks (invalid characters, reserved names, control codes, etc.) is only applied during the upload path. On the download path, only character encoding validation is performed. This makes sense because items that exist online presumably already passed server-side validation.

6. **No path length validation.** The reference does not check against OneDrive's 400-character path length limit. This is a gap we should address.

7. **The `skip_dir` segment matching (non-strict mode) is broad.** Without strict matching, a skip_dir pattern matches against every individual path component. This can lead to unintended exclusions if a common directory name is used as a pattern.

8. **Space handling in wild2regex maps space to `\s`.** This means a space in a pattern will match any whitespace character, not just a literal space. This could be surprising for users.

9. **sync_list anywhere rules are expensive.** The documentation explicitly warns that rules without a leading `/` cause exhaustive scanning of all online and local folders. Our implementation should consider optimizations for this case.

10. **Resync is required after changing any filter.** The reference requires a `--resync` operation whenever any client-side filtering rule is changed. This is because the local database state may be inconsistent with the new filtering rules.

11. **The shadow validation is a good safety feature.** Detecting conflicting filter configurations at startup prevents confusing behavior where included paths are silently excluded. We should implement equivalent validation.

12. **sync_list exclusion rules do not break evaluation early for exact/parental matches.** When an exclusion exact-match or parental-match is found, the flag is set but rule evaluation continues. However, anywhere exclusion matches DO break immediately. This inconsistency could lead to situations where a later inclusion rule could override an earlier exclusion for exact/parental matches (if the inclusion breaks the loop first), but not for anywhere matches.
