# OneDrive Filtering

OneDrive has no server-side filtering concept. All file/folder filtering is a client-side design decision.

OneDrive's naming restrictions and path length limits are documented in [onedrive-sync-behavior.md](onedrive-sync-behavior.md#naming-restrictions). For the project's filtering design, see `spec/design/sync-observation.md`.

This file exists as a placeholder in the reference layer. If no additional external filtering facts emerge, it should be deleted and its slot removed from the architecture.
