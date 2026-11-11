package devtool

import (
	"fmt"
	"os"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// benchPrimaryDriveEnvVar names the drive a live benchmark runs on.
const benchPrimaryDriveEnvVar = "ONEDRIVE_TEST_DRIVE"

type benchLiveConfig struct {
	PrimaryDrive string
}

func loadBenchLiveConfig(repoRoot string) (benchLiveConfig, error) {
	// The live suites' one .env reader, so the benchmark and the suites read
	// the same files the same way.
	if err := testutil.LoadTestEnvFiles(repoRoot); err != nil {
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
