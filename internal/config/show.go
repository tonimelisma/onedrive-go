package config

import (
	"fmt"
	"io"
	"strings"
)

// RenderEffective writes the resolved configuration as a human-readable
// annotated summary to w. This powers the "config show" command, giving
// users visibility into the effective values after all four override layers
// (defaults -> file -> env -> CLI) have been applied.
func RenderEffective(rp *ResolvedProfile, w io.Writer) error {
	ew := &errWriter{w: w}

	ew.printf("# Effective configuration for profile %q\n\n", rp.Name)

	renderProfileSection(ew, rp)
	renderFilterSection(ew, &rp.Filter)
	renderTransfersSection(ew, &rp.Transfers)
	renderSafetySection(ew, &rp.Safety)
	renderSyncSection(ew, &rp.Sync)
	renderLoggingSection(ew, &rp.Logging)
	renderNetworkSection(ew, &rp.Network)

	return ew.err
}

// errWriter wraps an io.Writer and captures the first write error.
// Subsequent writes after an error are no-ops, so callers can chain
// printf calls without checking each one individually.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}

	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}

func renderProfileSection(ew *errWriter, rp *ResolvedProfile) {
	ew.printf("[profile]\n")
	ew.printf("  name          = %q\n", rp.Name)
	ew.printf("  account_type  = %q\n", rp.AccountType)
	ew.printf("  sync_dir      = %q\n", rp.SyncDir)
	ew.printf("  remote_path   = %q\n", rp.RemotePath)

	if rp.DriveID != "" {
		ew.printf("  drive_id      = %q\n", rp.DriveID)
	}

	if rp.AzureADEndpoint != "" {
		ew.printf("  azure_ad_endpoint = %q\n", rp.AzureADEndpoint)
	}

	if rp.AzureTenantID != "" {
		ew.printf("  azure_tenant_id   = %q\n", rp.AzureTenantID)
	}

	ew.printf("\n")
}

func renderFilterSection(ew *errWriter, f *FilterConfig) {
	ew.printf("[filter]\n")
	ew.printf("  skip_dotfiles  = %t\n", f.SkipDotfiles)
	ew.printf("  skip_symlinks  = %t\n", f.SkipSymlinks)
	ew.printf("  max_file_size  = %q\n", f.MaxFileSize)
	ew.printf("  ignore_marker  = %q\n", f.IgnoreMarker)

	if len(f.SkipFiles) > 0 {
		ew.printf("  skip_files     = [%s]\n", joinQuoted(f.SkipFiles))
	}

	if len(f.SkipDirs) > 0 {
		ew.printf("  skip_dirs      = [%s]\n", joinQuoted(f.SkipDirs))
	}

	if len(f.SyncPaths) > 0 {
		ew.printf("  sync_paths     = [%s]\n", joinQuoted(f.SyncPaths))
	}

	ew.printf("\n")
}

func renderTransfersSection(ew *errWriter, t *TransfersConfig) {
	ew.printf("[transfers]\n")
	ew.printf("  parallel_downloads = %d\n", t.ParallelDownloads)
	ew.printf("  parallel_uploads   = %d\n", t.ParallelUploads)
	ew.printf("  parallel_checkers  = %d\n", t.ParallelCheckers)
	ew.printf("  chunk_size         = %q\n", t.ChunkSize)
	ew.printf("  bandwidth_limit    = %q\n", t.BandwidthLimit)
	ew.printf("  transfer_order     = %q\n", t.TransferOrder)
	ew.printf("\n")
}

func renderSafetySection(ew *errWriter, s *SafetyConfig) {
	ew.printf("[safety]\n")
	ew.printf("  big_delete_threshold  = %d\n", s.BigDeleteThreshold)
	ew.printf("  big_delete_percentage = %d\n", s.BigDeletePercentage)
	ew.printf("  big_delete_min_items  = %d\n", s.BigDeleteMinItems)
	ew.printf("  min_free_space        = %q\n", s.MinFreeSpace)
	ew.printf("  use_recycle_bin       = %t\n", s.UseRecycleBin)
	ew.printf("  use_local_trash       = %t\n", s.UseLocalTrash)
	ew.printf("  sync_dir_permissions  = %q\n", s.SyncDirPermissions)
	ew.printf("  sync_file_permissions = %q\n", s.SyncFilePermissions)
	ew.printf("  tombstone_retention_days = %d\n", s.TombstoneRetentionDays)
	ew.printf("\n")
}

func renderSyncSection(ew *errWriter, s *SyncConfig) {
	ew.printf("[sync]\n")
	ew.printf("  poll_interval              = %q\n", s.PollInterval)
	ew.printf("  fullscan_frequency         = %d\n", s.FullscanFrequency)
	ew.printf("  websocket                  = %t\n", s.Websocket)
	ew.printf("  conflict_strategy          = %q\n", s.ConflictStrategy)
	ew.printf("  conflict_reminder_interval = %q\n", s.ConflictReminderInterval)
	ew.printf("  dry_run                    = %t\n", s.DryRun)
	ew.printf("  verify_interval            = %q\n", s.VerifyInterval)
	ew.printf("  shutdown_timeout           = %q\n", s.ShutdownTimeout)
	ew.printf("\n")
}

func renderLoggingSection(ew *errWriter, l *LoggingConfig) {
	ew.printf("[logging]\n")
	ew.printf("  log_level          = %q\n", l.LogLevel)

	if l.LogFile != "" {
		ew.printf("  log_file           = %q\n", l.LogFile)
	}

	ew.printf("  log_format         = %q\n", l.LogFormat)
	ew.printf("  log_retention_days = %d\n", l.LogRetentionDays)
	ew.printf("\n")
}

func renderNetworkSection(ew *errWriter, n *NetworkConfig) {
	ew.printf("[network]\n")
	ew.printf("  connect_timeout = %q\n", n.ConnectTimeout)
	ew.printf("  data_timeout    = %q\n", n.DataTimeout)

	if n.UserAgent != "" {
		ew.printf("  user_agent      = %q\n", n.UserAgent)
	}

	ew.printf("  force_http_11   = %t\n", n.ForceHTTP11)
}

// joinQuoted formats a string slice as comma-separated quoted values.
func joinQuoted(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}

	return strings.Join(quoted, ", ")
}
