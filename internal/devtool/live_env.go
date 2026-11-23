package devtool

import (
	"fmt"
	"io"
	"os"

	"github.com/tonimelisma/onedrive-go/testutil/liveenv"
)

// benchPrimaryDriveEnvVar names the drive a live benchmark runs on.
const benchPrimaryDriveEnvVar = "ONEDRIVE_TEST_DRIVE"

type benchLiveConfig struct {
	PrimaryDrive string
}

func loadBenchLiveConfig(repoRoot string) (benchLiveConfig, error) {
	// The live suites' .env rule, so the benchmark and the suites read the
	// same files the same way; devtool opens them itself, so it links no
	// product filesystem package.
	if err := liveenv.LoadEnvFiles(openEnvFile, repoRoot); err != nil {
		return benchLiveConfig{}, fmt.Errorf("load live benchmark config: %w", err)
	}

	primaryDrive := os.Getenv(benchPrimaryDriveEnvVar)
	if primaryDrive == "" {
		return benchLiveConfig{}, fmt.Errorf("load live benchmark config: %s not set", benchPrimaryDriveEnvVar)
	}

	return benchLiveConfig{
		PrimaryDrive: primaryDrive,
	}, nil
}

// openEnvFile is liveenv's file capability for devtool: a plain read of a
// file of the repository the tool runs in.
func openEnvFile(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}

	return f, nil
}
