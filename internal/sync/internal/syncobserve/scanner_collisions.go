// Case and Unicode-form collision detection over one scan's entries
// (R-2.12.1, R-2.13.2).

package syncobserve

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// DetectCaseCollisions finds the new and changed entries whose name collides
// with another entry of the same directory on disk: names OneDrive treats as
// one (case, Unicode form, ignored characters, R-2.12.1). Each collider is
// removed from events and returned as a SkippedItem; uploading both would
// make one silently overwrite the other.
//
// names is every entry the scan found on disk (ScanNames). Only on-disk
// rivals count: a synced name that is gone is the old name of a case-only
// rename or a delete, which the planner orders, not a collision. An
// unchanged synced entry has no event and gets no finding, so it keeps
// syncing while the new name beside it waits (R-2.12.2). Two on-disk entries
// with one stored form (an NFC and an NFD twin) collide even when one of them
// is synced (R-2.13.2).
//
// O(n) time, O(n) memory. Pure function, no side effects.
func DetectCaseCollisions(
	events []syncdomain.LocalEntry, names ScanNames,
) (clean []syncdomain.LocalEntry, collisions []syncdomain.SkippedItem) {
	if len(events) == 0 {
		return nil, nil
	}

	changed := make(map[string]bool, len(events))
	for i := range events {
		changed[events[i].Path] = true
	}

	colliderSet := make(map[int]struct{})
	findings := make(map[int]nameCollision)
	for i := range events {
		d := names[filepath.Dir(events[i].Path)]
		if d == nil {
			continue
		}
		others, sameForm := d.rivals(filepath.Base(events[i].Path))
		if len(others) == 0 && !sameForm {
			continue
		}
		colliderSet[i] = struct{}{}
		findings[i] = nameCollision{others: others, sameForm: sameForm}
	}

	// Suppress children of colliding directories.
	childColliderSet, collidingDirPrefixes := suppressDirectoryChildren(events, colliderSet)

	if len(colliderSet) == 0 {
		return events, nil
	}

	collisions = buildCollisionSkippedItems(events, findings, changed, childColliderSet, collidingDirPrefixes)

	clean = make([]syncdomain.LocalEntry, 0, len(events)-len(colliderSet))
	for i := range events {
		if _, collider := colliderSet[i]; !collider {
			clean = append(clean, events[i])
		}
	}

	return clean, collisions
}

// nameCollision is what DetectCaseCollisions found for one entry: the
// on-disk spellings of its rivals, and whether a twin shares its stored form.
type nameCollision struct {
	others   []string
	sameForm bool
}

// suppressDirectoryChildren marks children of colliding directories as colliders.
// They can't be uploaded to a folder that won't exist on OneDrive.
func suppressDirectoryChildren(
	events []syncdomain.LocalEntry, colliderSet map[int]struct{},
) (childColliderSet map[int]struct{}, collidingDirPrefixes []string) {
	childColliderSet = make(map[int]struct{})

	for idx := range colliderSet {
		if events[idx].ItemType == syncdomain.ItemTypeFolder {
			collidingDirPrefixes = append(collidingDirPrefixes, events[idx].Path+"/")
		}
	}

	for i := range events {
		if _, already := colliderSet[i]; already {
			continue
		}

		for _, prefix := range collidingDirPrefixes {
			if strings.HasPrefix(events[i].Path, prefix) {
				colliderSet[i] = struct{}{}
				childColliderSet[i] = struct{}{}

				break
			}
		}
	}

	return childColliderSet, collidingDirPrefixes
}

// buildCollisionSkippedItems constructs one SkippedItem per colliding stored
// path, with a Detail naming the other collider(s), and one per child of a
// colliding directory.
func buildCollisionSkippedItems(
	events []syncdomain.LocalEntry,
	findings map[int]nameCollision,
	changed map[string]bool,
	childColliderSet map[int]struct{},
	collidingDirPrefixes []string,
) []syncdomain.SkippedItem {
	collisions := make([]syncdomain.SkippedItem, 0, len(findings)+len(childColliderSet))

	seen := make(map[string]bool, len(findings))
	for idx := range events {
		found, ok := findings[idx]
		if !ok {
			continue
		}
		ev := &events[idx]
		if seen[ev.Path] {
			continue // the same stored path is reported once
		}
		seen[ev.Path] = true
		collisions = append(collisions, syncdomain.SkippedItem{
			Path:   ev.Path,
			Reason: syncdomain.IssueCaseCollision,
			Detail: nameCollisionDetail(ev.Path, found, changed),
		})
	}

	// Child collisions — distinct Detail indicating the parent directory collision.
	for idx := range events {
		if _, isChild := childColliderSet[idx]; !isChild {
			continue
		}
		ev := &events[idx]

		parentDir := ""
		for _, prefix := range collidingDirPrefixes {
			if strings.HasPrefix(ev.Path, prefix) {
				parentDir = strings.TrimSuffix(prefix, "/")

				break
			}
		}

		collisions = append(collisions, syncdomain.SkippedItem{
			Path:   ev.Path,
			Reason: syncdomain.IssueCaseCollision,
			Detail: fmt.Sprintf("parent directory %q has a case collision",
				filepath.Base(parentDir)),
		})
	}

	return collisions
}

// nameCollisionDetail is the finding's detail for the entry stored at path. A
// rival with no event of its own is a synced file the user did not change.
func nameCollisionDetail(path string, found nameCollision, changed map[string]bool) string {
	dir, name := filepath.Dir(path), filepath.Base(path)
	others := make([]string, 0, len(found.others))
	rivalChanged := false
	for _, other := range found.others {
		stored := namekey.Stored(other)
		others = append(others, stored)
		if changed[filepath.Join(dir, stored)] {
			rivalChanged = true
		}
	}

	if found.sameForm {
		detail := sameUnicodeFormCollisionDetail
		if len(others) > 0 {
			detail += "; it also conflicts with " + strings.Join(others, ", ")
		}

		return detail
	}

	prefix := "conflicts with synced file"
	if rivalChanged {
		prefix = "conflicts with"
	}

	return collisionDetail(prefix, name, others)
}

// collisionDetail names the other colliding file(s) and what to do. When the
// names differ in characters OneDrive ignores when comparing names (emoji and
// anything else beyond U+FFFF), the visible difference would leave the user
// puzzled about why `a.txt` and `a😀.txt` collide, so the detail says why.
func collisionDetail(prefix, name string, others []string) string {
	detail := prefix + " " + strings.Join(others, ", ")
	for _, other := range others {
		if namekey.DiffersInIgnoredCharacters(name, other) {
			return fmt.Sprintf("%s: OneDrive ignores emoji and other characters outside the Basic Multilingual Plane "+
				"when it compares names, so %q and %q are one name there; rename one of them", detail, name, other)
		}
	}

	if len(others) == 0 {
		return detail
	}

	return fmt.Sprintf("%s: OneDrive treats %q and %q as the same name; rename one of them", detail, name, others[0])
}

// sameUnicodeFormCollisionDetail is the finding's detail for two entries of
// one folder whose names differ only in Unicode form: stored paths are NFC
// (R-2.13.1), so both have one path, and naming the other would print the
// same name twice.
const sameUnicodeFormCollisionDetail = "another file here has the same name in a different Unicode form " +
	"(for example é typed as e plus an accent); rename one of them"
