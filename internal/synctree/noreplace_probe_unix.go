//go:build unix

package synctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// probeArtifactSuffix names the files ProbeNoReplaceSupport makes in the
// sync folder, after detachTempPrefix and the random part. Local observation
// ignores the name (IsProbeArtifactName): the probe removes its files before
// it returns, so only a run that died inside it leaves one, an empty file.
const probeArtifactSuffix = ".probe"

// probeListBatch is how many entries of the sync folder one read of its
// listing takes, so a folder of any size is read in bounded pieces.
const probeListBatch = 256

// probeFilePerm is the mode of the empty probe file.
const probeFilePerm = 0o600

// IsProbeArtifactName reports whether name is a file ProbeNoReplaceSupport
// made.
func IsProbeArtifactName(name string) bool {
	return strings.HasPrefix(name, detachTempPrefix) && strings.HasSuffix(name, probeArtifactSuffix)
}

// NoReplaceSupport is what the sync folder's filesystem offers a no-replace
// rename (RenameNoReplace) and the put-back of a set-aside file
// (linkBackNoReplace).
type NoReplaceSupport struct {
	// Flag is the system's no-replace rename flag (renameat2
	// RENAME_NOREPLACE, renameatx_np RENAME_EXCL).
	Flag bool
	// HardLinks is a hard link of a file in the sync folder.
	HardLinks bool
}

// PutBackCanReplace reports that the filesystem offers neither: a file is
// put back, and a rename made without replacing, by a check that the name is
// free and a plain rename, so a file saved at that name between the two is
// replaced (renameCheckedAt). FAT-family filesystems are such.
func (s NoReplaceSupport) PutBackCanReplace() bool {
	return !s.Flag && !s.HardLinks
}

// ProbeNoReplaceSupport finds what the sync folder's filesystem offers a
// no-replace rename: it makes an empty file in the sync folder, links it to
// a second name and renames it to a third with the no-replace flag, through
// the same descriptor calls the renames use (rootOps), and removes what it
// made. A refusal that only says the filesystem has no hard links
// (localpath.IsLinkUnsupported), or no flag, is the answer; any other
// failure is the error.
func (r *Root) ProbeNoReplaceSupport() (support NoReplaceSupport, err error) {
	if r.memory != nil {
		// The in-memory volume's rename refuses to replace by itself.
		return NoReplaceSupport{Flag: true}, nil
	}

	var names [probeNameCount]string
	for i := range names {
		if names[i], err = randomName(detachTempPrefix, probeArtifactSuffix); err != nil {
			return NoReplaceSupport{}, err
		}
	}

	dir, err := r.openParentFolder(names[0])
	if err != nil {
		return NoReplaceSupport{}, fmt.Errorf("opening sync folder %s for the rename probe: %w", r.dir, err)
	}
	defer closeDirectory(dir)

	if sweepErr := r.removeLeftoverProbeFiles(dir); sweepErr != nil {
		return NoReplaceSupport{}, sweepErr
	}

	createErr := createProbeFile(dir, names[0])
	defer func() {
		if removeErr := r.removeProbeFiles(dir, names[:]); err == nil {
			err = removeErr
		}
	}()
	if createErr != nil {
		return NoReplaceSupport{}, createErr
	}

	return r.probeNoReplaceAt(dir, names[0], names[1], names[2])
}

// probeNameCount is the probe's file and the two names it is linked and
// renamed to.
const probeNameCount = 3

func createProbeFile(dir *os.File, name string) error {
	file, err := openatExclusive(dir, name, probeFilePerm)
	if err != nil {
		return fmt.Errorf("creating the rename probe %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing the rename probe %s: %w", name, err)
	}

	return nil
}

// removeLeftoverProbeFiles removes the files an earlier probe left in the
// sync folder when its run died inside it: every entry of the folder named
// as a probe's file (IsProbeArtifactName). Only a probe makes such a name, so
// nothing else is removed; a name that is not a file is left alone.
func (r *Root) removeLeftoverProbeFiles(dir *os.File) error {
	var leftovers []string
	for {
		entries, err := dir.ReadDir(probeListBatch)
		for _, entry := range entries {
			if entry.Type().IsRegular() && IsProbeArtifactName(entry.Name()) {
				leftovers = append(leftovers, entry.Name())
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(entries) == 0) {
			break
		}
		if err != nil {
			return fmt.Errorf("listing sync folder %s for leftover rename probes: %w", r.dir, err)
		}
	}

	return r.removeProbeFiles(dir, leftovers)
}

// removeProbeFiles removes every name the probe may have made; a name it
// never made is no failure.
func (r *Root) removeProbeFiles(dir *os.File, names []string) error {
	var first error
	for _, name := range names {
		if err := r.ops.unlink(dir, name); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = fmt.Errorf("removing the rename probe %s: %w", name, err)
		}
	}

	return first
}

func (r *Root) probeNoReplaceAt(dir *os.File, source, linked, renamed string) (NoReplaceSupport, error) {
	var support NoReplaceSupport
	linkErr := r.ops.link(dir, source, dir, linked)
	switch {
	case linkErr == nil:
		support.HardLinks = true
	case localpath.IsLinkUnsupported(linkErr):
	default:
		return NoReplaceSupport{}, fmt.Errorf("linking the rename probe %s: %w", source, linkErr)
	}

	refused, renameErr := r.ops.renameExclusive(dir, source, dir, renamed)
	if !refused && renameErr != nil {
		return NoReplaceSupport{}, fmt.Errorf("renaming the rename probe %s: %w", source, renameErr)
	}
	support.Flag = !refused

	return support, nil
}
