package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// cidFileSuffix is the file extension for legacy CID-based file-name tests.
// Token files still use this extension in steady state.
const cidFileSuffix = ".json"

// discoverCIDFiles scans dir for token files matching
// "token_{type}_{email}.json" and returns the canonical IDs extracted from
// filenames. A data directory that does not exist holds no files; one that
// cannot be listed is an error, never "no files".
func discoverCIDFiles(dir string, logger *slog.Logger) ([]driveid.CanonicalID, error) {
	return discoverCIDFilesWithIO(dir, logger, defaultConfigIO())
}

func discoverCIDFilesWithIO(dir string, logger *slog.Logger, io configIO) ([]driveid.CanonicalID, error) {
	if dir == "" {
		return nil, nil
	}

	entries, err := readDataDirForDiscovery(dir, logger, io, "token")
	if err != nil || entries == nil {
		return nil, err
	}

	var ids []driveid.CanonicalID

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		if !strings.HasPrefix(name, "token_") || !strings.HasSuffix(name, cidFileSuffix) {
			continue
		}

		// Strip prefix and suffix, then split on first "_" to recover
		// {type} and {email}. Emails may contain underscores, so only the
		// first underscore separates type from email.
		inner := strings.TrimPrefix(name, "token_")
		inner = strings.TrimSuffix(inner, cidFileSuffix)

		parts := strings.SplitN(inner, "_", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			logger.Debug("skipping malformed filename", "name", name)

			continue
		}

		cid, err := driveid.Construct(parts[0], parts[1])
		if err != nil {
			logger.Debug("skipping file with invalid drive type", "name", name, "error", err)

			continue
		}

		ids = append(ids, cid)
	}

	slices.SortFunc(ids, func(a, b driveid.CanonicalID) int {
		return cmp.Compare(a.String(), b.String())
	})
	logger.Debug("file discovery complete", "dir", dir, "count", len(ids))

	return ids, nil
}

// readDataDirForDiscovery lists the data directory for one discovery kind. A
// missing directory is the ordinary first-run state and yields no entries. Any
// other failure (permission denied, I/O error, a file where the directory
// should be) is reported once here, at Warn, and returned, so the callers
// that used to read it as "no accounts" or "no state databases" can say that
// the data directory could not be read instead.
func readDataDirForDiscovery(dir string, logger *slog.Logger, io configIO, kind string) ([]fs.DirEntry, error) {
	entries, err := io.readManagedDir(dir)
	if err == nil {
		return entries, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		logger.Debug("data directory does not exist yet; nothing to discover",
			"dir", dir, "kind", kind)

		return nil, nil
	}

	logger.Warn("cannot read the data directory",
		"dir", dir, "kind", kind, "error", err)

	return nil, fmt.Errorf("cannot read the data directory %s: %w", dir, err)
}

// discoverFilesForEmail scans dir for files matching "{prefix}*{suffix}" that
// contain the given email at an underscore boundary. Returns full file paths.
// This is the shared implementation behind DiscoverStateDBsForEmail and
// other email-scoped managed-file discovery. An unreadable data directory is
// an error, not an empty result (see readDataDirForDiscovery).
func discoverFilesForEmail(dir, prefix, suffix, email string, logger *slog.Logger) ([]string, error) {
	return discoverFilesForEmailWithIO(dir, prefix, suffix, email, logger, defaultConfigIO())
}

func discoverFilesForEmailWithIO(
	dir, prefix, suffix, email string,
	logger *slog.Logger,
	io configIO,
) ([]string, error) {
	if dir == "" || email == "" {
		return nil, nil
	}

	entries, err := readDataDirForDiscovery(dir, logger, io, prefix+"*"+suffix)
	if err != nil || entries == nil {
		return nil, err
	}

	needle := "_" + email

	var paths []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}

		if !containsEmailBoundary(name, needle) {
			continue
		}

		paths = append(paths, filepath.Join(dir, name))
	}

	return paths, nil
}

// containsEmailBoundary checks if name contains needle ("_email") at an
// underscore boundary. The character after email must be ".", "_", or end-of-string
// to prevent substring collisions (e.g. "a@b.com" matching "ba@b.com").
func containsEmailBoundary(name, needle string) bool {
	idx := strings.Index(name, needle)
	if idx < 0 {
		return false
	}

	afterEmail := idx + len(needle)
	if afterEmail >= len(name) {
		return true
	}

	c := name[afterEmail]

	return c == '.' || c == '_'
}
