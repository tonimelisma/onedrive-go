package testutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// DriveRoles names the configured live test drives by role: drive-1 for the
// primary drive, drive-2 for the secondary one. What the live suites record,
// the timing and quirk summaries and the per-test debug logs, goes into
// artifacts of a public repository's CI, where log masking does not reach, so
// every recorder writes through Redact and records roles, never a drive
// identifier or an account address. The zero value redacts nothing.
type DriveRoles struct {
	roles    map[string]string
	replacer *strings.Replacer
}

const (
	driveRolePrefix     = "drive-"
	driveAccountSuffix  = "-account"
	percentEncodedAt    = "%40"
	percentEncodedColon = "%3A"
)

// NewDriveRoles names driveIDs, in order, drive-1, drive-2 and so on. An
// empty drive keeps its place and is skipped.
func NewDriveRoles(driveIDs ...string) DriveRoles {
	roles := map[string]string{}
	replacements := map[string]string{}
	for i, driveID := range driveIDs {
		if driveID == "" {
			continue
		}
		role := driveRolePrefix + strconv.Itoa(i+1)
		roles[driveID] = role
		for _, spelling := range driveSpellings(driveID) {
			replacements[spelling] = role
		}
		if address := driveEmailFromCanonicalID(driveID); address != "" {
			for _, spelling := range driveSpellings(address) {
				replacements[spelling] = role + driveAccountSuffix
			}
		}
	}
	if len(replacements) == 0 {
		return DriveRoles{}
	}

	// Longest first: a drive identifier is replaced whole before the account
	// address inside it.
	olds := make([]string, 0, len(replacements))
	for old := range replacements {
		olds = append(olds, old)
	}
	sort.Slice(olds, func(i, j int) bool {
		if len(olds[i]) != len(olds[j]) {
			return len(olds[i]) > len(olds[j])
		}
		return olds[i] < olds[j]
	})
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		pairs = append(pairs, old, replacements[old])
	}

	return DriveRoles{roles: roles, replacer: strings.NewReplacer(pairs...)}
}

// driveSpellings lists the ways a recorded line can spell a drive identifier
// or an account address: as written, with its colons as underscores (test
// and token file names), lower-cased, and percent-encoded as Graph URLs spell
// it.
func driveSpellings(value string) []string {
	var spellings []string
	for _, form := range []string{value, strings.ReplaceAll(value, ":", "_")} {
		for _, cased := range []string{form, strings.ToLower(form)} {
			encoded := strings.NewReplacer("@", percentEncodedAt, ":", percentEncodedColon).Replace(cased)
			spellings = append(spellings, cased, encoded)
		}
	}

	return spellings
}

// Role returns the role of a configured drive, a redacted spelling of any
// other text, and the empty string for an empty drive.
func (r DriveRoles) Role(driveID string) string {
	if role, ok := r.roles[driveID]; ok {
		return role
	}

	return r.Redact(driveID)
}

// Redact replaces every spelling of a configured drive in text with its
// role, and every spelling of its account address with the role followed by
// -account.
func (r DriveRoles) Redact(text string) string {
	if r.replacer == nil {
		return text
	}

	return r.replacer.Replace(text)
}

// AccountFinding is one line of a tracked file that spells a test account.
type AccountFinding struct {
	File string
	Line int
	// Account is the account's role, such as drive-1-account: a finding
	// never carries the address.
	Account string
}

// String names the file, the line and the account's role.
func (f AccountFinding) String() string {
	return f.File + ":" + strconv.Itoa(f.Line) + " spells " + f.Account
}

// FindTestAccounts lists every line of the files, relative to root, that
// holds the account address of one of driveIDs (the configured drives and
// the allowlist entries) as written, in any case, or percent-encoded. Each
// account is named by its role in driveIDs' order. A binary file is read like
// any other, its line being the count of newlines before the address; a
// tracked file the checkout lacks is skipped.
func FindTestAccounts(root string, files []string, driveIDs []string) ([]AccountFinding, error) {
	unique := []string{}
	seen := map[string]bool{}
	for _, driveID := range driveIDs {
		driveID = strings.TrimSpace(driveID)
		if driveID == "" || seen[driveID] {
			continue
		}
		seen[driveID] = true
		unique = append(unique, driveID)
	}
	roles := NewDriveRoles(unique...)

	type account struct {
		role      string
		spellings []string
	}
	var accounts []account
	for _, driveID := range unique {
		// A shared selector's address is its second field only.
		address, _, _ := strings.Cut(strings.ToLower(driveEmailFromCanonicalID(driveID)), ":")
		if address == "" {
			continue
		}
		encoded := strings.NewReplacer("@", strings.ToLower(percentEncodedAt), ":", strings.ToLower(percentEncodedColon)).Replace(address)
		accounts = append(accounts, account{role: roles.Redact(address), spellings: []string{address, encoded}})
	}

	var findings []AccountFinding
	for _, file := range files {
		data, err := localpath.ReadFile(filepath.Join(root, file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		for number, line := range strings.Split(strings.ToLower(string(data)), "\n") {
			for _, candidate := range accounts {
				for _, spelling := range candidate.spellings {
					if strings.Contains(line, spelling) {
						findings = append(findings, AccountFinding{File: file, Line: number + 1, Account: candidate.role})
						break
					}
				}
			}
		}
	}

	return findings, nil
}
