package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// AddDriveRequest names one drive to enroll and what enrolling it needs.
// `drive add` sends one for an account's own drive or for a shared folder;
// login sends one for the account's primary drive, with Login set.
type AddDriveRequest struct {
	ConfigPath string
	DataDir    string
	Drive      driveid.CanonicalID
	// Owner is the signed-in account a shared drive is shared with. It is
	// zero for every other drive, whose owner follows from its canonical ID.
	Owner driveid.CanonicalID
	// DisplayName names a shared drive whose section has no display_name
	// yet. The caller resolves it (a network lookup) before the transaction;
	// "" means the default name. Other drives ignore it.
	DisplayName string
	// Login is what a sign-in learned about the account. Only login sets it,
	// and then Drive is the account's primary drive.
	Login *AccountLogin
	// MakeSyncDir creates the drive's sync directory from its configured
	// sync_dir, which may start with "~/". The CLI supplies it because it
	// owns how that directory is made.
	MakeSyncDir func(syncDir string) error
}

// AddDriveResult says what an enrollment found and did.
type AddDriveResult struct {
	SyncDir     string // the drive's sync_dir as the config holds it
	DisplayName string // the name the drive goes by
	Added       bool   // this call wrote the drive's section
	// Completed means the section was already there and this call supplied
	// what it lacked: its sync_dir, a shared drive's display_name, its
	// catalog record (for login also the account's), or its sync directory.
	// Such a section was written by hand or by an earlier version of the
	// client, or its directory was removed since.
	Completed bool
}

// AddDrive enrolls one drive as a single transaction: it takes the config
// lock and then the catalog lock, reads both files and decides everything
// from that one snapshot, then creates the sync directory, writes the
// catalog, and writes the config once. The config write is the commit point:
// when it fails, the catalog bytes read under the lock are written back, so a
// failure leaves config and catalog as they were. A failure after the sync
// directory was created leaves that directory, empty: it is not removed,
// since it may be a folder the user already had. A process killed part way
// leaves at most an empty sync directory and a catalog record without a
// section, and running the same enrollment again completes it. An enrollment
// that is already complete writes nothing.
func AddDrive(req *AddDriveRequest, logger *slog.Logger) (AddDriveResult, error) {
	return addDrive(req, defaultConfigIO(), defaultConfigLocker(), time.Now(), logger)
}

func addDrive(
	req *AddDriveRequest,
	io configIO,
	locker configLocker,
	now time.Time,
	logger *slog.Logger,
) (AddDriveResult, error) {
	if err := req.validate(); err != nil {
		return AddDriveResult{}, err
	}

	var result AddDriveResult
	err := locker.withConfigAndCatalogLocks(req.ConfigPath, CatalogPathForDataDir(req.DataDir), func() error {
		var enrollErr error
		result, enrollErr = enrollLocked(req, io, now)
		return enrollErr
	})
	if err != nil {
		return AddDriveResult{}, err
	}

	logger.Info("drive enrolled",
		slog.String("drive", req.Drive.String()),
		slog.String("sync_dir", result.SyncDir),
		slog.Bool("added", result.Added),
		slog.Bool("completed", result.Completed),
	)

	return result, nil
}

func (req *AddDriveRequest) validate() error {
	switch {
	case req.Drive.IsZero():
		return errors.New("no drive given")
	case req.ConfigPath == "":
		return errors.New("no config file path")
	case req.DataDir == "":
		return errors.New("no data directory")
	case req.MakeSyncDir == nil:
		return errors.New("no way to create the sync directory")
	case req.Drive.IsShared():
		if req.Owner.IsZero() {
			return errors.New("a shared drive needs the account it is shared with")
		}
		if req.Login != nil {
			return errors.New("login enrolls an account's primary drive, not a shared drive")
		}
	case !req.Owner.IsZero():
		return errors.New("only a shared drive names its owner; this drive's owner follows from its ID")
	case accountCIDForDrive(req.Drive).IsZero():
		// Personal and business drives own themselves and a SharePoint
		// library belongs to the business account of its email; an ID that
		// yields neither has no account to record as its owner.
		return fmt.Errorf("drive %s: its ID names no account that could own it", req.Drive)
	case req.Login != nil && !accountCIDForDrive(req.Drive).Equal(req.Drive):
		return errors.New("login enrolls an account's primary drive")
	}

	return nil
}

// enrollmentSnapshot is config and catalog as read under both locks, raw and
// decoded. The raw catalog bytes are what a failed commit writes back.
type enrollmentSnapshot struct {
	configData    []byte
	configExists  bool
	config        *Config
	catalogData   []byte
	catalogExists bool
	catalog       *Catalog
}

// enrollmentPlan is everything the transaction decided before it changes
// anything. newConfig and newCatalog are nil when that file needs no change.
type enrollmentPlan struct {
	syncDir       string
	displayName   string
	newConfig     []byte
	newCatalog    []byte
	sectionExists bool // the drive's section was already there; false means this call adds it
	sectionLacked bool // the section is missing a key this call writes
	recordLacked  bool // the catalog is missing a record this call writes
}

func enrollLocked(req *AddDriveRequest, io configIO, now time.Time) (AddDriveResult, error) {
	snapshot, err := readEnrollmentSnapshot(req, io)
	if err != nil {
		return AddDriveResult{}, err
	}

	plan, err := planEnrollment(req, &snapshot, io, now)
	if err != nil {
		return AddDriveResult{}, err
	}

	// Only a missing directory matters here, to report it as completed; any
	// other failure to stat it is MakeSyncDir's to report, one step later.
	_, statErr := io.statLocalPath(ExpandTilde(plan.syncDir))
	dirMissing := errors.Is(statErr, os.ErrNotExist)

	if commitErr := commitEnrollment(req, io, &snapshot, &plan); commitErr != nil {
		return AddDriveResult{}, commitErr
	}

	return AddDriveResult{
		SyncDir:     plan.syncDir,
		DisplayName: plan.displayName,
		Added:       !plan.sectionExists,
		Completed:   plan.sectionExists && (plan.sectionLacked || plan.recordLacked || dirMissing),
	}, nil
}

func readEnrollmentSnapshot(req *AddDriveRequest, io configIO) (enrollmentSnapshot, error) {
	var snapshot enrollmentSnapshot

	configData, configExists, err := readManagedFileIfExists(io, req.ConfigPath)
	if err != nil {
		return enrollmentSnapshot{}, fmt.Errorf("reading config file: %w", err)
	}
	snapshot.configData, snapshot.configExists = configData, configExists
	snapshot.config = DefaultConfig()
	if configExists {
		cfg, parseErr := newConfigLoader(io).parseStrict(req.ConfigPath, configData)
		if parseErr != nil {
			return enrollmentSnapshot{}, markInvalidConfig(fmt.Errorf("loading config: %w", parseErr))
		}
		snapshot.config = cfg
	}

	catalogData, catalogExists, err := readManagedFileIfExists(io, CatalogPathForDataDir(req.DataDir))
	if err != nil {
		return enrollmentSnapshot{}, fmt.Errorf("reading catalog: %w", err)
	}
	snapshot.catalogData, snapshot.catalogExists = catalogData, catalogExists
	snapshot.catalog = DefaultCatalog()
	if catalogExists {
		catalog, decodeErr := decodeCatalog(CatalogPathForDataDir(req.DataDir), catalogData)
		if decodeErr != nil {
			return enrollmentSnapshot{}, decodeErr
		}
		snapshot.catalog = catalog
	}

	return snapshot, nil
}

// planEnrollment decides the enrollment from the snapshot alone: the drive's
// name and sync directory, the catalog after this drive's record is written,
// and the config text after its section is appended or completed. A config
// text that strict loading would reject (two drives sharing a sync
// directory, say) fails here, before anything is written.
func planEnrollment(req *AddDriveRequest, snapshot *enrollmentSnapshot, io configIO, now time.Time) (enrollmentPlan, error) {
	section, sectionExists := snapshot.config.Drives[req.Drive]
	plan := enrollmentPlan{
		sectionExists: sectionExists,
		displayName:   enrolledDisplayName(req, &section),
	}

	catalogAfter := cloneCatalog(snapshot.catalog)
	plan.recordLacked = catalogLacksRecords(snapshot.catalog, req)
	applyEnrollmentToCatalog(catalogAfter, req, plan.displayName, now)

	plan.syncDir = section.SyncDir
	if plan.syncDir == "" {
		orgName, nameForDir := accountNames(catalogAfter, req.Drive)
		if req.Drive.IsShared() {
			nameForDir = plan.displayName
		}
		plan.syncDir = DefaultSyncDir(req.Drive, orgName, nameForDir, otherSyncDirs(snapshot.config, catalogAfter, req.Drive))
		if plan.syncDir == "" {
			return enrollmentPlan{}, fmt.Errorf("no default sync directory for drive type %q", req.Drive.DriveType())
		}
	}

	var err error
	plan.newConfig, plan.sectionLacked, err = plannedConfigText(req, snapshot, sectionExists, &section, plan.syncDir, plan.displayName)
	if err != nil {
		return enrollmentPlan{}, err
	}
	if plan.newConfig != nil {
		if _, checkErr := newConfigLoader(io).parseStrict(req.ConfigPath, plan.newConfig); checkErr != nil {
			return enrollmentPlan{}, fmt.Errorf("checking the config with the drive added: %w", checkErr)
		}
	}

	plan.newCatalog, err = plannedCatalogBytes(CatalogPathForDataDir(req.DataDir), snapshot.catalog, catalogAfter)
	if err != nil {
		return enrollmentPlan{}, err
	}

	return plan, nil
}

// enrolledDisplayName is the name the drive goes by after enrollment: the
// section's own display_name, else (shared drives) the name the caller
// resolved, else the default.
func enrolledDisplayName(req *AddDriveRequest, section *Drive) string {
	switch {
	case section.DisplayName != "":
		return section.DisplayName
	case req.Drive.IsShared() && req.DisplayName != "":
		return req.DisplayName
	default:
		return DefaultDisplayName(req.Drive)
	}
}

// catalogLacksRecords reports whether the catalog is missing a record this
// enrollment writes: the drive's, and for login the account's.
func catalogLacksRecords(catalog *Catalog, req *AddDriveRequest) bool {
	if _, found := catalog.DriveByCanonicalID(req.Drive); !found {
		return true
	}
	if req.Login != nil {
		if _, found := catalog.AccountByCanonicalID(req.Drive); !found {
			return true
		}
	}

	return false
}

func applyEnrollmentToCatalog(catalog *Catalog, req *AddDriveRequest, displayName string, now time.Time) {
	switch {
	case req.Login != nil:
		catalog.recordLogin(req.Drive, req.Login, now)
	case req.Drive.IsShared():
		catalog.registerSharedDrive(req.Drive, req.Owner, displayName)
	default:
		catalog.registerOwnedDrive(req.Drive, accountCIDForDrive(req.Drive))
	}
}

// plannedConfigText is the config text after this enrollment, or nil when the
// section is already complete. A new section carries sync_dir, and a shared
// drive's display_name, in one append; an existing section gets only the
// keys it lacks, and the second result says it lacked any.
func plannedConfigText(
	req *AddDriveRequest,
	snapshot *enrollmentSnapshot,
	sectionExists bool,
	section *Drive,
	syncDir string,
	displayName string,
) ([]byte, bool, error) {
	writeSyncDir := !sectionExists || section.SyncDir == ""
	writeDisplayName := req.Drive.IsShared() && (!sectionExists || section.DisplayName == "")

	var keys []driveKey
	if writeSyncDir {
		key, err := newDriveKey(syncDirKey, syncDir)
		if err != nil {
			return nil, false, err
		}
		keys = append(keys, key)
	}
	if writeDisplayName {
		key, err := newDriveKey(displayNameKey, displayName)
		if err != nil {
			return nil, false, err
		}
		keys = append(keys, key)
	}

	if !sectionExists {
		text, err := withDriveSectionAppended(snapshot.configData, snapshot.configExists, req.Drive, keys)
		return text, false, err
	}
	if len(keys) == 0 {
		return nil, false, nil
	}

	text, err := withDriveKeysSet(snapshot.configData, req.Drive, keys)
	return text, true, err
}

// plannedCatalogBytes is the catalog file after this enrollment, or nil when
// its records already say everything this enrollment would write.
func plannedCatalogBytes(path string, before, after *Catalog) ([]byte, error) {
	beforeData, err := encodeCatalog(path, cloneCatalog(before))
	if err != nil {
		return nil, err
	}
	afterData, err := encodeCatalog(path, after)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(beforeData, afterData) {
		return nil, nil
	}

	return afterData, nil
}

func cloneCatalog(catalog *Catalog) *Catalog {
	return &Catalog{
		SchemaVersion: catalog.SchemaVersion,
		Accounts:      maps.Clone(catalog.Accounts),
		Drives:        maps.Clone(catalog.Drives),
	}
}

// commitEnrollment carries out what planEnrollment decided: the sync
// directory, then the catalog, then the config, the commit point. A config
// write that fails puts the catalog back as it was read, still under the
// lock; when that also fails, both errors are returned and what is left is a
// catalog record without a section, which the next enrollment of the drive
// completes.
func commitEnrollment(req *AddDriveRequest, io configIO, snapshot *enrollmentSnapshot, plan *enrollmentPlan) error {
	if err := req.MakeSyncDir(plan.syncDir); err != nil {
		return fmt.Errorf("creating sync directory: %w", err)
	}

	catalogPath := CatalogPathForDataDir(req.DataDir)
	if plan.newCatalog != nil {
		if err := io.writeManagedFile(catalogPath, plan.newCatalog); err != nil {
			return fmt.Errorf("writing catalog: %w", err)
		}
	}

	if plan.newConfig == nil {
		return nil
	}

	configErr := io.writeManagedFile(req.ConfigPath, plan.newConfig)
	if configErr == nil {
		return nil
	}

	commitErr := fmt.Errorf("writing config: %w", configErr)
	if plan.newCatalog == nil {
		return commitErr
	}

	if restoreErr := restoreCatalogSnapshot(io, catalogPath, snapshot); restoreErr != nil {
		return errors.Join(commitErr, fmt.Errorf("putting the catalog back: %w", restoreErr))
	}

	return commitErr
}

func restoreCatalogSnapshot(io configIO, catalogPath string, snapshot *enrollmentSnapshot) error {
	if !snapshot.catalogExists {
		return io.removeManagedFile(catalogPath)
	}

	return io.writeManagedFile(catalogPath, snapshot.catalogData)
}
