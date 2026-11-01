# Security policy

## Supported versions

onedrive-go has no releases yet. Security fixes go to the `main` branch; build
from the latest `main` to get them.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately
through GitHub: open the repository's **Security** tab and choose **Report a
vulnerability** ([direct link](https://github.com/tonimelisma/onedrive-go/security/advisories/new)).

Include what you found, how to reproduce it, and what an attacker could do with
it. You should get an acknowledgement within a week.

## What counts

Examples of problems we want to hear about:

- anything that lets another local user or process read your OneDrive sign-in
  tokens, or act on your OneDrive through onedrive-go;
- a file name or path from OneDrive that makes onedrive-go write outside the
  sync folder;
- secrets (tokens, pre-authenticated download or upload URLs) written to logs
  or console output;
- a way to make onedrive-go delete or overwrite data it should not touch.

## What onedrive-go stores

- Sign-in tokens are stored per account as `token_*.json` files in the data
  directory (`~/.local/share/onedrive-go/` on Linux,
  `~/Library/Application Support/onedrive-go/` on macOS) with mode `0600`.
- onedrive-go asks Microsoft for the `Files.ReadWrite.All`, `User.Read` and
  `offline_access` permissions.
- Files sync deleted or replaced on this computer are kept for 30 days in a
  per-drive trash under the same data directory (`onedrive-go trash list`),
  with the data directory's owner-only permissions.
- The local control socket used by `status`, `pause` and config reloads is
  created with mode `0600` in the data directory (or, when that path is too
  long for a Unix socket, in a short hash-named directory under the system
  temporary directory).
- The config file (`config.toml`) names the signed-in accounts in its section
  headers and is written with mode `0600`.
- The `log_file` is created with mode `0600`, and any directory onedrive-go
  creates for it with mode `0700`.

Logs written with `--debug` or `log_file` contain file names and account
details. Review them before sharing.
