package syncfixture

import (
	"fmt"
	"io"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func CopyFileContent(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // test copies its own temp tree
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()) //nolint:gosec // test copies its own temp tree
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", dst, closeErr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}

	return nil
}

// AllCrashPoints is every crash point crash_points.go declares, each of which
// crashRecoveryScenario reaches. TestModel_CrashAtSeededPointsRecovers draws
// from it by index, so a new point goes last.
func AllCrashPoints() []string {
	return []string{
		syncdomain.CrashPointRemoteMoveDone,
		syncdomain.CrashPointLocalMoveDone,
		syncdomain.CrashPointUploadCreatedUnrecorded,
		syncdomain.CrashPointUploadRemoteDone,
		syncdomain.CrashPointDownloadRenamed,
		syncdomain.CrashPointLocalDeleteTrashed,
		syncdomain.CrashPointRemoteDeleteDone,
		syncdomain.CrashPointCommitObservation,
		syncdomain.CrashPointCommitOutcome,
		syncdomain.CrashPointLocalMoveDestinationTrashed,
	}
}

func CountOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}

	return n
}
