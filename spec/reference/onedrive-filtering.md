# OneDrive Filtering

OneDrive does not provide a general server-side sync-filtering API. Filtering is
client-side sync policy layered on top of Graph/provider truth. Direct file
operations in this project therefore keep exposing provider-visible items, while
the sync engine applies configured content visibility before planning.

External behavior we intentionally mirror:

- Microsoft documents invalid names, invalid characters, SharePoint root
  `forms`, temporary files, `.ds_store`/`desktop.ini` behavior, size/path limits,
  and lack of support for syncing through symlinks or junctions:
  <https://support.microsoft.com/en-us/office/restrictions-and-limitations-in-onedrive-and-sharepoint-64883a5d-228e-48f5-b3d2-eb39e07630fa>
- Microsoft Graph models OneNote notebooks and similar compound provider
  objects through the `package` facet rather than ordinary file/folder facets:
  <https://learn.microsoft.com/en-us/graph/api/resources/package?view=graph-rest-1.0>
- rclone ignores symlinks by default unless link handling is explicitly enabled:
  <https://rclone.org/docs/#links>
- Syncthing ignore rules are explicit user policy and ignored files are not
  synchronized; its `(?d)` delete marker is useful precedent for separating
  "ignored" from "safe to delete":
  <https://docs.syncthing.net/users/ignoring.html>
- abraunegg's OneDrive client exposes explicit `skip_dir`, `skip_file`,
  `skip_dotfiles`, `sync_list`, and symlink-related options. It also has
  `.nosync` behavior, but this project deliberately uses config keys instead of
  marker files:
  <https://github.com/abraunegg/onedrive/blob/master/docs/usage.md>

Project decisions:

- `included_dirs`, `ignored_dirs`, `ignored_paths`, `ignore_dotfiles`, and
  `ignore_junk_files` are sync-only visibility policy.
- Local observation persists only visible/admissible `local_state`.
- Remote observation persists raw manageable `remote_state`; the planner builds
  filtered remote views from that raw truth.
- Invalid OneDrive names, path length, size limits, case collisions, read-denied
  paths, and hash failures are local observation issues for visible content, not
  general-purpose ignore rules.
- OneNote/package items and Personal Vault are remote/provider exclusions owned
  by sync observation.
- `.nosync` and other marker-looking files are ordinary files unless matched by
  explicit config.
